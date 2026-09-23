package engine

import "sort"

// accountEntry is the internal mutable state of one account.
//
// Cash accounting:
//
//	cash         = total settled cash
//	reservedCash = cash frozen by resting buy orders (price * openQty each)
//	available cash = cash - reservedCash
//
// Position accounting per symbol:
//
//	positions[sym]         = net settled position
//	reservedShares[sym]    = shares frozen by resting sell orders
//	available shares       = positions - reservedShares
//
// Reservations guarantee that resting orders can never drive an account
// negative; trades merely consume what was frozen (for resting makers) or
// check/settle immediately (for incoming takers).
type accountEntry struct {
	id             string
	cash           int64
	reservedCash   int64
	positions      map[string]int64
	reservedShares map[string]int64
	openOrders     map[string]*Order
}

// Ledger holds account cash, positions and open orders. All access happens
// under the engine's global lock; Ledger has no lock of its own.
type Ledger struct {
	accounts map[string]*accountEntry
}

// NewLedger creates a ledger and seeds every account named in initialPositions
// (plus accountsWithCash) with cash and initial positions.
func NewLedger(initialCash int64, initialPositions map[string]map[string]int64) *Ledger {
	l := &Ledger{accounts: make(map[string]*accountEntry)}
	names := map[string]struct{}{}
	for a := range initialPositions {
		names[a] = struct{}{}
	}
	for a := range names {
		acct := l.ensureAccount(a)
		acct.cash = initialCash
		for sym, qty := range initialPositions[a] {
			acct.positions[sym] = qty
		}
	}
	return l
}

// ensureAccount returns the account, creating an empty one if missing.
func (l *Ledger) ensureAccount(account string) *accountEntry {
	a := l.accounts[account]
	if a == nil {
		a = &accountEntry{
			id:             account,
			positions:      make(map[string]int64),
			reservedShares: make(map[string]int64),
			openOrders:     make(map[string]*Order),
		}
		l.accounts[account] = a
	}
	return a
}

// HasAccount reports whether an account exists.
func (l *Ledger) HasAccount(account string) bool {
	return l.accounts[account] != nil
}

// Accounts returns sorted account IDs.
func (l *Ledger) Accounts() []string {
	out := make([]string, 0, len(l.accounts))
	for a := range l.accounts {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// CreateAccount registers a new account with starting cash/positions.
func (l *Ledger) CreateAccount(account string, cash int64, positions map[string]int64) *RejectError {
	if l.HasAccount(account) {
		return rejectf(RejectUnknownAccount, "account %q already exists", account)
	}
	a := l.ensureAccount(account)
	a.cash = cash
	for sym, qty := range positions {
		a.positions[sym] = qty
	}
	return nil
}

// Cash returns total settled cash of an account.
func (l *Ledger) Cash(account string) int64 {
	return l.accounts[account].cash
}

// AvailableCash returns cash not frozen by resting buy orders.
func (l *Ledger) AvailableCash(account string) int64 {
	a := l.accounts[account]
	return a.cash - a.reservedCash
}

// Position returns net settled position in a symbol.
func (l *Ledger) Position(account, symbol string) int64 {
	return l.accounts[account].positions[symbol]
}

// AvailableShares returns shares not frozen by resting sell orders.
func (l *Ledger) AvailableShares(account, symbol string) int64 {
	a := l.accounts[account]
	return a.positions[symbol] - a.reservedShares[symbol]
}

// OpenOrder returns an active resting order by ID.
func (l *Ledger) OpenOrder(id string) (*Order, bool) {
	for _, a := range l.accounts {
		if o := a.openOrders[id]; o != nil {
			return o, true
		}
	}
	return nil, false
}

// addOpenOrder registers an accepted resting order with its owner.
func (l *Ledger) addOpenOrder(o *Order) {
	l.accounts[o.Account].openOrders[o.ID] = o
}

// removeOpenOrder unregisters a resting order.
func (l *Ledger) removeOpenOrder(id string) {
	for _, a := range l.accounts {
		if _, ok := a.openOrders[id]; ok {
			delete(a.openOrders, id)
			return
		}
	}
}

// openNotional returns sum(price*openQty) of an account's resting orders.
func (l *Ledger) openNotional(account string) int64 {
	var n int64
	for _, o := range l.accounts[account].openOrders {
		n += o.Price * o.OpenQty
	}
	return n
}

// globalOpenNotional returns sum(price*openQty) over all resting orders.
func (l *Ledger) globalOpenNotional() int64 {
	var n int64
	for _, a := range l.accounts {
		for _, o := range a.openOrders {
			n += o.Price * o.OpenQty
		}
	}
	return n
}

// accountNetOpenQty returns signed resting net qty (buy +, sell -) of an
// account for one symbol.
func (l *Ledger) accountNetOpenQty(account, symbol string) int64 {
	var q int64
	for _, o := range l.accounts[account].openOrders {
		if o.Symbol != symbol {
			continue
		}
		if o.Side == SideBuy {
			q += o.OpenQty
		} else {
			q -= o.OpenQty
		}
	}
	return q
}

// ---- reservation primitives (called under the engine global lock) ----

func (a *accountEntry) availCash() int64 { return a.cash - a.reservedCash }

func (a *accountEntry) availShares(symbol string) int64 {
	return a.positions[symbol] - a.reservedShares[symbol]
}

// reserveBuy freezes cash for a resting buy order.
func (l *Ledger) reserveBuy(account string, amount int64) {
	a := l.accounts[account]
	a.reservedCash += amount
}

// releaseBuy unfreezes cash (cancellation / quantity reduction).
func (l *Ledger) releaseBuy(account string, amount int64) {
	a := l.accounts[account]
	a.reservedCash -= amount
}

// reserveSell freezes shares for a resting sell order.
func (l *Ledger) reserveSell(account, symbol string, qty int64) {
	a := l.accounts[account]
	a.reservedShares[symbol] += qty
}

// releaseSell unfreezes shares.
func (l *Ledger) releaseSell(account, symbol string, qty int64) {
	a := l.accounts[account]
	a.reservedShares[symbol] -= qty
}

// settleMaker applies the reservation-consuming leg of a trade for a resting
// maker: consume its frozen funds/shares and move cash/positions. The counter
// side (taker) is handled by settleTaker.
//
// cost = price*qty is always exactly reserved for a resting maker at its
// own limit price.
func (l *Ledger) settleMaker(maker *Order, price, qty int64) {
	a := l.accounts[maker.Account]
	cost := price * qty
	if maker.Side == SideBuy {
		a.reservedCash -= cost
		a.cash -= cost
		a.positions[maker.Symbol] += qty
	} else {
		a.reservedShares[maker.Symbol] -= qty
		a.positions[maker.Symbol] -= qty
		a.cash += cost
	}
}

// settleTaker settles an incoming order that was not reserved: it requires
// immediately available funds/shares (checked by the caller) and moves them.
func (l *Ledger) settleTaker(taker *Order, price, qty int64) {
	a := l.accounts[taker.Account]
	cost := price * qty
	if taker.Side == SideBuy {
		a.cash -= cost
		a.positions[taker.Symbol] += qty
	} else {
		a.positions[taker.Symbol] -= qty
		a.cash += cost
	}
}

// Snapshot returns a public copy of an account's state.
func (l *Ledger) Snapshot(account string) AccountState {
	a := l.accounts[account]
	st := AccountState{
		ID:                account,
		Cash:              a.cash,
		ReservedCash:      a.reservedCash,
		Positions:         copyMap(a.positions),
		ReservedPositions: copyMap(a.reservedShares),
		OpenOrders:        len(a.openOrders),
	}
	return st
}

func copyMap(m map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
