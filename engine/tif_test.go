package engine

import (
	"path/filepath"
	"reflect"
	"testing"
)

// mustAccount snapshots one account or fails.
func mustAccount(t *testing.T, e *Engine, id string) *AccountState {
	t.Helper()
	st, err := e.AccountSnapshot(id)
	if err != nil {
		t.Fatalf("snapshot %s: %v", id, err)
	}
	t.Logf("ACCOUNT %s cash=%d reservedCash=%d pos=%v reservedPos=%v openOrders=%d",
		id, st.Cash, st.ReservedCash, st.Positions, st.ReservedPositions, st.OpenOrders)
	return st
}

func mustBook(t *testing.T, e *Engine, symbol string) BookSnapshot {
	t.Helper()
	_, books, err := e.Snapshot("A")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	b := books[symbol]
	t.Logf("BOOK %s bids=%v asks=%v", symbol, b.Bids, b.Asks)
	return b
}

func mustOrderStatus(t *testing.T, e *Engine, id string) (OrderStatus, *Order) {
	t.Helper()
	o, ok := e.Order(id)
	if !ok {
		t.Fatalf("order %s not found", id)
	}
	st := o.Status()
	t.Logf("ORDER %s status=%s filled=%d open=%d terminalCancel=%v",
		id, st, o.FilledQty, o.OpenQty, o.TerminalCancel)
	return st, o
}

// TestIOCLimitPartialFillKillsRemainder: IOC limit buy crosses one level,
// remainder is killed at commit, never rests, never reserves.
func TestIOCLimitPartialFillKillsRemainder(t *testing.T) {
	e := newTestEngine(t, "A", "B", "C")
	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "B", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 5})
	mustSubmit(t, e, OrderRequest{ID: "s2", Account: "C", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 101, Qty: 5})

	r := mustSubmit(t, e, OrderRequest{ID: "ioc1", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, TIF: TIFIOC, Price: 100, Qty: 8})
	if r.Status != StatusCanceled {
		t.Fatalf("IOC partial must end CANCELED, got %s", r.Status)
	}
	if r.Resting {
		t.Fatalf("IOC order must never rest")
	}
	if len(r.Trades) != 1 || r.Trades[0].Qty != 5 || r.Trades[0].Price != 100 {
		t.Fatalf("want one trade 5@100, got %+v", r.Trades)
	}

	st, o := mustOrderStatus(t, e, "ioc1")
	if st != StatusCanceled || o.FilledQty != 5 || o.OpenQty != 0 {
		t.Fatalf("stored IOC order: status=%s filled=%d open=%d", st, o.FilledQty, o.OpenQty)
	}

	// Book: level 100 consumed, level 101 untouched, no bid appeared.
	b := mustBook(t, e, "X")
	if len(b.Bids) != 0 {
		t.Fatalf("IOC remainder must not rest, bids=%v", b.Bids)
	}
	if !reflect.DeepEqual(b.Asks, []BookLevel{{Price: 101, Qty: 5}}) {
		t.Fatalf("asks should be [101x5], got %v", b.Asks)
	}

	// Account: only the filled 5@100 settled; nothing stays reserved.
	a := mustAccount(t, e, "A")
	if a.Cash != 1_000_000-500 || a.ReservedCash != 0 || a.Positions["X"] != 10000+5 {
		t.Fatalf("buyer state wrong: %+v", a)
	}
	mk := mustAccount(t, e, "B")
	if mk.Cash != 1_000_000+500 || mk.ReservedPositions["X"] != 0 {
		t.Fatalf("maker state wrong: %+v", mk)
	}
}

// TestIOCLimitFullFillAndNoCross: full fill reports FILLED; a non-crossing
// IOC is canceled without trades and without touching anything.
func TestIOCLimitFullFillAndNoCross(t *testing.T) {
	e := newTestEngine(t, "A", "B", "C")
	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "B", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 5})
	mustSubmit(t, e, OrderRequest{ID: "s2", Account: "C", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 101, Qty: 5})

	r := mustSubmit(t, e, OrderRequest{ID: "iocF", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, TIF: TIFIOC, Price: 101, Qty: 10})
	if r.Status != StatusFilled || len(r.Trades) != 2 {
		t.Fatalf("IOC full fill: status=%s trades=%d", r.Status, len(r.Trades))
	}
	st, _ := mustOrderStatus(t, e, "iocF")
	if st != StatusFilled {
		t.Fatalf("stored full-fill IOC must be FILLED, got %s", st)
	}
	a := mustAccount(t, e, "A")
	if a.ReservedCash != 0 || a.Cash != 1_000_000-(500+505) {
		t.Fatalf("full-fill IOC must settle exactly fill cost: %+v", a)
	}

	before := mustAccount(t, e, "A")
	r2 := mustSubmit(t, e, OrderRequest{ID: "iocN", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, TIF: TIFIOC, Price: 99, Qty: 5})
	if r2.Status != StatusCanceled || len(r2.Trades) != 0 {
		t.Fatalf("non-crossing IOC: status=%s trades=%d", r2.Status, len(r2.Trades))
	}
	after := mustAccount(t, e, "A")
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("zero-fill IOC must not change account: before=%+v after=%+v", before, after)
	}
	if b := mustBook(t, e, "X"); len(b.Bids) != 0 || len(b.Asks) != 0 {
		t.Fatalf("book must be empty after IOC sweep, got %+v", b)
	}
}

// TestIOCSellPartialSettlesOnlyFilledShares: a sell IOC settles only the
// filled shares; the remainder is killed without freezing the position.
func TestIOCSellPartialSettlesOnlyFilledShares(t *testing.T) {
	e := newTestEngine(t, "A", "S")
	mustSubmit(t, e, OrderRequest{ID: "b1", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 4})

	r := mustSubmit(t, e, OrderRequest{ID: "iocS", Account: "S", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, TIF: TIFIOC, Price: 100, Qty: 10})
	if r.Status != StatusCanceled || len(r.Trades) != 1 || r.Trades[0].Qty != 4 {
		t.Fatalf("sell IOC partial: status=%s trades=%+v", r.Status, r.Trades)
	}
	s := mustAccount(t, e, "S")
	if s.Positions["X"] != 10000-4 || s.ReservedPositions["X"] != 0 || s.Cash != 1_000_000+400 {
		t.Fatalf("seller must settle only 4 shares and reserve nothing: %+v", s)
	}
	if b := mustBook(t, e, "X"); len(b.Bids) != 0 || len(b.Asks) != 0 {
		t.Fatalf("book must be empty, got %+v", b)
	}
}

// TestFOKLimitFullyFillable: FOK sweeps multiple levels atomically.
func TestFOKLimitFullyFillable(t *testing.T) {
	e := newTestEngine(t, "A", "B", "C")
	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "B", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 3})
	mustSubmit(t, e, OrderRequest{ID: "s2", Account: "C", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 101, Qty: 4})

	r := mustSubmit(t, e, OrderRequest{ID: "fok1", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, TIF: TIFFOK, Price: 101, Qty: 7})
	if r.Status != StatusFilled || len(r.Trades) != 2 {
		t.Fatalf("FOK fill: status=%s trades=%d", r.Status, len(r.Trades))
	}
	if r.Resting {
		t.Fatalf("FOK must never rest")
	}
	a := mustAccount(t, e, "A")
	if a.Cash != 1_000_000-(3*100+4*101) || a.ReservedCash != 0 || a.Positions["X"] != 10000+7 {
		t.Fatalf("FOK buyer state wrong: %+v", a)
	}
	if b := mustBook(t, e, "X"); len(b.Asks) != 0 || len(b.Bids) != 0 {
		t.Fatalf("book must be empty after FOK sweep, got %+v", b)
	}
}

// TestFOKUnfillableLeavesNoTrace: insufficient depth rejects the whole order;
// book, accounts and order registry stay exactly as before (limit & market).
func TestFOKUnfillableLeavesNoTrace(t *testing.T) {
	e := newTestEngine(t, "A", "B")
	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "B", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 3})

	beforeA := mustAccount(t, e, "A")
	beforeB := mustAccount(t, e, "B")
	beforeBook := mustBook(t, e, "X")

	mustReject(t, e, OrderRequest{ID: "fokL", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, TIF: TIFFOK, Price: 100, Qty: 4}, RejectFOKUnfillable)
	mustReject(t, e, OrderRequest{ID: "fokM", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeMarket, TIF: TIFFOK, Qty: 4}, RejectFOKUnfillable)
	// A sell FOK beyond own position must also leave no trace.
	mustReject(t, e, OrderRequest{ID: "fokS", Account: "A", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, TIF: TIFFOK, Price: 99, Qty: 4}, RejectFOKUnfillable)

	if a := mustAccount(t, e, "A"); !reflect.DeepEqual(a, beforeA) {
		t.Fatalf("FOK reject changed account A: before=%+v after=%+v", beforeA, a)
	}
	if b := mustAccount(t, e, "B"); !reflect.DeepEqual(b, beforeB) {
		t.Fatalf("FOK reject changed account B: before=%+v after=%+v", beforeB, b)
	}
	if b := mustBook(t, e, "X"); !reflect.DeepEqual(b, beforeBook) {
		t.Fatalf("FOK reject changed book: before=%+v after=%+v", beforeBook, b)
	}
	if _, ok := e.Order("fokL"); ok {
		t.Fatalf("rejected FOK must not register an order")
	}
}

// TestFOKSelfTradeRejected: self-crossing FOK reports SELF_TRADE, no fills.
func TestFOKSelfTradeRejected(t *testing.T) {
	e := newTestEngine(t, "A")
	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "A", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 5})
	mustReject(t, e, OrderRequest{ID: "fok1", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, TIF: TIFFOK, Price: 100, Qty: 5}, RejectSelfTrade)
	mustReject(t, e, OrderRequest{ID: "ioc1", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, TIF: TIFIOC, Price: 100, Qty: 5}, RejectSelfTrade)
	b := mustBook(t, e, "X")
	if !reflect.DeepEqual(b.Asks, []BookLevel{{Price: 100, Qty: 5}}) {
		t.Fatalf("self-trade reject must keep book, got %v", b.Asks)
	}
}

// TestInvalidTIFRejected: unknown time-in-force values are rejected statically.
func TestInvalidTIFRejected(t *testing.T) {
	e := newTestEngine(t, "A")
	mustReject(t, e, OrderRequest{ID: "bad", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, TIF: TimeInForce(99), Price: 100, Qty: 1}, RejectInvalidTIF)
}

// TestLimitPartialRestThenFillAndCancel: an ordinary limit taker that only
// partially crosses keeps its remainder at the original price; the remainder
// can later be filled, and a partially filled remainder can be canceled.
// Order status, book and reservations are asserted at every step.
func TestLimitPartialRestThenFillAndCancel(t *testing.T) {
	e := newTestEngine(t, "A", "B", "C", "D", "E")

	// A posts a big bid; B eats only part of it.
	mustSubmit(t, e, OrderRequest{ID: "a1", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 10})
	mustSubmit(t, e, OrderRequest{ID: "b1", Account: "B", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 4})

	st, o := mustOrderStatus(t, e, "a1")
	if st != StatusPartiallyFilled || o.OpenQty != 6 || o.FilledQty != 4 {
		t.Fatalf("a1 must be partially filled 4/10, got %s open=%d filled=%d", st, o.OpenQty, o.FilledQty)
	}
	b := mustBook(t, e, "X")
	if !reflect.DeepEqual(b.Bids, []BookLevel{{Price: 100, Qty: 6}}) {
		t.Fatalf("remainder 6 must rest at 100, got %v", b.Bids)
	}
	a := mustAccount(t, e, "A")
	if a.ReservedCash != 600 || a.Cash != 1_000_000-400 || a.Positions["X"] != 10000+4 {
		t.Fatalf("A after partial: %+v", a)
	}

	// C finishes the remainder at the same price.
	mustSubmit(t, e, OrderRequest{ID: "c1", Account: "C", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 6})
	st, _ = mustOrderStatus(t, e, "a1")
	if st != StatusFilled {
		t.Fatalf("a1 must be FILLED, got %s", st)
	}
	a = mustAccount(t, e, "A")
	if a.ReservedCash != 0 || a.Cash != 1_000_000-1000 || a.Positions["X"] != 10000+10 {
		t.Fatalf("A after full fill: %+v", a)
	}
	if b := mustBook(t, e, "X"); len(b.Bids) != 0 {
		t.Fatalf("book must have no bids, got %v", b.Bids)
	}

	// D's bid is partially eaten, then the resting remainder is canceled.
	mustSubmit(t, e, OrderRequest{ID: "d1", Account: "D", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 101, Qty: 8})
	mustSubmit(t, e, OrderRequest{ID: "e1", Account: "E", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 101, Qty: 3})
	st, o = mustOrderStatus(t, e, "d1")
	if st != StatusPartiallyFilled || o.OpenQty != 5 {
		t.Fatalf("d1 must rest 5 after partial, got %s open=%d", st, o.OpenQty)
	}
	d := mustAccount(t, e, "D")
	if d.ReservedCash != 505 || d.Cash != 1_000_000-303 {
		t.Fatalf("D after partial: %+v", d)
	}
	if _, err := e.Cancel("d1"); err != nil {
		t.Fatalf("cancel d1: %v", err)
	}
	st, _ = mustOrderStatus(t, e, "d1")
	if st != StatusCanceled {
		t.Fatalf("canceled partial must be CANCELED (not FILLED), got %s", st)
	}
	d = mustAccount(t, e, "D")
	if d.ReservedCash != 0 || d.Cash != 1_000_000-303 || d.Positions["X"] != 10000+3 {
		t.Fatalf("D after cancel must keep fills and release reserve: %+v", d)
	}
	if b := mustBook(t, e, "X"); len(b.Bids) != 0 || len(b.Asks) != 0 {
		t.Fatalf("book must be empty after cancel, got %+v", b)
	}
}

// TestTIFReplayConsistency: IOC/FOK instructions journal a complete,
// replayable trail; replay and recovery agree with the live run.
func TestTIFReplayConsistency(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.bin")
	positions := map[string]map[string]int64{
		"A": {"X": 1000}, "B": {"X": 1000}, "C": {"X": 1000},
	}
	cfg := EngineConfig{JournalPath: path, InitialCash: 1_000_000, InitialPositions: positions}
	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}

	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "B", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 5})
	mustSubmit(t, e, OrderRequest{ID: "s2", Account: "C", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 101, Qty: 5})
	mustSubmit(t, e, OrderRequest{ID: "ioc1", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, TIF: TIFIOC, Price: 100, Qty: 8})
	mustSubmit(t, e, OrderRequest{ID: "fok1", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, TIF: TIFFOK, Price: 101, Qty: 5})
	mustReject(t, e, OrderRequest{ID: "fok2", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, TIF: TIFFOK, Price: 101, Qty: 1}, RejectFOKUnfillable)
	mustSubmit(t, e, OrderRequest{ID: "ioc2", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, TIF: TIFIOC, Price: 99, Qty: 3})

	liveA := mustAccount(t, e, "A")
	liveB := mustAccount(t, e, "B")
	liveC := mustAccount(t, e, "C")
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	genesis := EngineConfig{InitialCash: 1_000_000, InitialPositions: positions}
	st, err := ReplayWithGenesis(path, genesis)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	t.Logf("REPLAY %d events, %d trades", len(st.Events), len(st.Trades))
	for _, ev := range st.Events {
		t.Logf("  EVENT seq=%d type=%s order=%s tif=%s reason=%s", ev.Seq, ev.Type, ev.OrderID, ev.TIF, ev.Reason)
	}
	if !reflect.DeepEqual(st.Accounts["A"], liveA) {
		t.Fatalf("replay A mismatch:\n live=%+v\n replay=%+v", liveA, st.Accounts["A"])
	}
	if !reflect.DeepEqual(st.Accounts["B"], liveB) || !reflect.DeepEqual(st.Accounts["C"], liveC) {
		t.Fatalf("replay maker accounts mismatch")
	}
	if len(st.Trades) != 2 {
		t.Fatalf("want 2 trades (IOC 5@100 + FOK 5@101), got %d", len(st.Trades))
	}
	if b := st.Books["X"]; b == nil || len(b.Bids) != 0 || len(b.Asks) != 0 {
		t.Fatalf("replayed book must be empty, got %+v", b)
	}
	// Replay is read-only and idempotent.
	st2, err := ReplayWithGenesis(path, genesis)
	if err != nil || !reflect.DeepEqual(st, st2) {
		t.Fatalf("repeated replay diverged: %v", err)
	}

	// Recovery: reopen on the same journal and keep trading; sequences and
	// state must continue seamlessly.
	e2, err := NewEngine(EngineConfig{JournalPath: path, InitialCash: 1_000_000, InitialPositions: positions})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	defer e2.Close()
	a := mustAccount(t, e2, "A")
	if !reflect.DeepEqual(a, liveA) {
		t.Fatalf("recovered account mismatch:\n live=%+v\n recovered=%+v", liveA, a)
	}
	mustSubmit(t, e2, OrderRequest{ID: "post", Account: "B", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 50, Qty: 1})
}
