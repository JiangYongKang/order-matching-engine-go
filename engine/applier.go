package engine

import (
	"fmt"
	"sort"
)

// replayView accumulates reconstructed state while replaying an event
// stream. Recovery inside NewEngine and the standalone Replay function use
// exactly this code path, so incremental runs and replay cannot diverge.
type replayView struct {
	ledger   *Ledger
	books    map[string]*Book
	orders   map[string]*Order // every order seen, keyed by ID
	rejected map[string]struct{}
	trades   []*Trade
	events   []*Event
}

func newReplayView() *replayView {
	return &replayView{
		ledger:   NewLedger(0, nil),
		books:    make(map[string]*Book),
		orders:   make(map[string]*Order),
		rejected: make(map[string]struct{}),
	}
}

func (v *replayView) book(symbol string) *Book {
	b := v.books[symbol]
	if b == nil {
		b = NewBook(symbol)
		v.books[symbol] = b
	}
	return b
}

// applyEvent mutates reconstructed state for one journal event.
func (v *replayView) applyEvent(ev *Event) error {
	v.events = append(v.events, ev)
	switch ev.Type {
	case EventAccepted:
		return v.applyAccepted(ev)
	case EventTrade:
		return v.applyTrade(ev)
	case EventCanceled:
		return v.applyCanceled(ev)
	case EventReplaced:
		return v.applyReplaced(ev)
	case EventExpired:
		return v.applyExpired(ev)
	case EventRejected:
		v.rejected[ev.OrderID] = struct{}{}
		return nil
	default:
		return fmt.Errorf("unknown event type %d at seq %d", ev.Type, ev.Seq)
	}
}

func (v *replayView) applyAccepted(ev *Event) error {
	if ev.Order == nil {
		return fmt.Errorf("accepted event %d missing order", ev.Seq)
	}
	o := ev.Order
	v.ledger.ensureAccount(o.Account)
	v.orders[o.ID] = o
	if o.OpenQty <= 0 {
		return nil
	}
	v.ledger.addOpenOrder(o)
	if o.Side == SideBuy {
		v.ledger.reserveBuy(o.Account, ev.ReservedCash)
	} else {
		v.ledger.reserveSell(o.Account, o.Symbol, ev.ReservedShares)
	}
	v.book(o.Symbol).Add(o)
	return nil
}

func (v *replayView) applyTrade(ev *Event) error {
	t := ev.Trade
	if t == nil {
		return fmt.Errorf("trade event %d missing payload", ev.Seq)
	}
	maker := v.orders[t.MakerOrderID]
	if maker == nil {
		return fmt.Errorf("trade %d references unknown maker order %s", t.Seq, t.MakerOrderID)
	}
	taker := v.orders[t.TakerOrderID]
	if taker == nil {
		// Incoming market / fully-crossed limit orders never rested; create
		// a transient order describing the taker side for settlement.
		v.ledger.ensureAccount(t.TakerAccount)
		taker = &Order{
			ID:      t.TakerOrderID,
			Account: t.TakerAccount,
			Symbol:  t.Symbol,
			Side:    opposite(maker.Side),
			Type:    OrderTypeMarket,
			Qty:     0,
		}
		v.orders[t.TakerOrderID] = taker
	}
	v.ledger.settleMaker(maker, t.Price, t.Qty)
	v.ledger.settleTaker(taker, t.Price, t.Qty)
	maker.FilledQty += t.Qty
	maker.OpenQty -= t.Qty
	taker.FilledQty += t.Qty
	if taker.Qty > 0 && taker.OpenQty > 0 {
		// Resting GTC taker crossing in its own leg: track its remainder.
		// Immediate (market/IOC/FOK) takers rest nothing and keep OpenQty 0
		// from acceptance; their remainder is closed by a later Expired event.
		taker.OpenQty -= t.Qty
	}
	if taker.Qty > 0 && taker.OpenQty == 0 && taker.terminal == 0 {
		// Resting limit taker fully consumed by its own crossing leg. For an
		// immediate taker the order is not in the book, so Remove is a no-op.
		v.book(taker.Symbol).Remove(taker)
		v.ledger.removeOpenOrder(taker.ID)
	}
	if maker.OpenQty == 0 {
		v.book(maker.Symbol).Remove(maker)
		v.ledger.removeOpenOrder(maker.ID)
	}
	v.trades = append(v.trades, t)
	return nil
}

func (v *replayView) applyCanceled(ev *Event) error {
	o := v.orders[ev.OrderID]
	if o == nil || o.OpenQty <= 0 {
		return fmt.Errorf("canceled event %d: order %s not active", ev.Seq, ev.OrderID)
	}
	if o.Side == SideBuy {
		v.ledger.releaseBuy(o.Account, o.Price*o.OpenQty)
	} else {
		v.ledger.releaseSell(o.Account, o.Symbol, o.OpenQty)
	}
	v.book(o.Symbol).Remove(o)
	v.ledger.removeOpenOrder(o.ID)
	o.OpenQty = 0
	o.terminal = StatusCanceled
	return nil
}

// applyExpired closes an immediate (IOC/market) order whose unfilled
// remainder was canceled at submission. Such an order never rested and froze
// nothing, so the book and ledger are untouched; the event only reconstructs
// its final CANCELED status (distinguishing "partially filled then expired"
// from a fully filled order after replay/recovery). LeftoverQty must match
// the order's remaining open quantity, which catches a corrupted stream.
func (v *replayView) applyExpired(ev *Event) error {
	o := v.orders[ev.OrderID]
	if o == nil {
		return fmt.Errorf("expired event %d: order %s unknown", ev.Seq, ev.OrderID)
	}
	if o.terminal != 0 {
		return fmt.Errorf("expired event %d: order %s already terminal (%s)", ev.Seq, ev.OrderID, o.terminal)
	}
	if ev.LeftoverQty <= 0 || ev.LeftoverQty != o.Qty-o.FilledQty-o.OpenQty {
		return fmt.Errorf("expired event %d: leftover %d does not match unfilled qty %d of %s",
			ev.Seq, ev.LeftoverQty, o.Qty-o.FilledQty-o.OpenQty, ev.OrderID)
	}
	o.OpenQty = 0
	o.terminal = StatusCanceled
	return nil
}

func (v *replayView) applyReplaced(ev *Event) error {
	old := v.orders[ev.OrderID]
	if ev.KeptPriority {
		if old == nil || old.OpenQty <= 0 {
			return fmt.Errorf("replaced event %d: order %s not active", ev.Seq, ev.OrderID)
		}
		// Same price: release the reservation tied to the previous quantity
		// and freeze for the amended one; position in the level is untouched.
		if old.Side == SideBuy {
			v.ledger.releaseBuy(old.Account, old.Price*old.OpenQty)
		} else {
			v.ledger.releaseSell(old.Account, old.Symbol, old.OpenQty)
		}
		old.OpenQty = ev.Qty
		old.Price = ev.Price
		if old.Side == SideBuy {
			v.ledger.reserveBuy(old.Account, ev.Price*ev.Qty)
		} else {
			v.ledger.reserveSell(old.Account, old.Symbol, ev.Qty)
		}
		return nil
	}
	// Price changed: the old order is withdrawn before the new order (same
	// ID) is re-accepted. Subsequent trade/accepted events handle the new leg.
	if old != nil && old.OpenQty > 0 {
		if old.Side == SideBuy {
			v.ledger.releaseBuy(old.Account, old.Price*old.OpenQty)
		} else {
			v.ledger.releaseSell(old.Account, old.Symbol, old.OpenQty)
		}
		v.book(old.Symbol).Remove(old)
		v.ledger.removeOpenOrder(old.ID)
	}
	if ev.Order != nil {
		v.orders[ev.OrderID] = ev.Order
	}
	return nil
}

// state builds the public ReplayState from accumulated state.
func (v *replayView) state() *ReplayState {
	st := &ReplayState{
		Accounts: make(map[string]*AccountState),
		Books:    make(map[string]*BookSnapshot),
		Trades:   v.trades,
		Events:   v.events,
	}
	for _, a := range v.ledger.Accounts() {
		snap := v.ledger.Snapshot(a)
		st.Accounts[a] = &snap
	}
	symbols := make([]string, 0, len(v.books))
	for sym := range v.books {
		symbols = append(symbols, sym)
	}
	sort.Strings(symbols)
	for _, sym := range symbols {
		snap := v.books[sym].Snapshot()
		st.Books[sym] = &snap
	}
	return st
}

// applyAll runs the stream on an empty view, enforcing strict sequence
// continuity (gaps or duplicates indicate a corrupted journal).
func applyAll(events []*Event) (*replayView, error) {
	return applyAllInto(events, newReplayView())
}

// applyAllInto runs the stream on a pre-seeded view (genesis cash/positions),
// so recovery after restart applies the journal exactly once on top of
// configuration balances.
func applyAllInto(events []*Event, v *replayView) (*replayView, error) {
	for i, ev := range events {
		if ev.Seq != int64(i+1) {
			return nil, fmt.Errorf("journal sequence broken at index %d: got seq %d, want %d", i, ev.Seq, i+1)
		}
		if err := v.applyEvent(ev); err != nil {
			return nil, err
		}
	}
	return v, nil
}
