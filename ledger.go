package matching

import (
	"errors"
	"math"
)

// ErrAccountExists 账户重复开立。
var ErrAccountExists = errors.New("account already exists")

// notional 计算 price*qty 并检测 int64 溢出。
func notional(price, qty int64) (int64, bool) {
	if price == 0 || qty == 0 {
		return 0, true
	}
	if qty > math.MaxInt64/price || qty < math.MinInt64/price {
		return 0, false
	}
	return price * qty, true
}

// Ledger 管理全部账户状态，所有方法仅做资金/持仓的冻结、释放与结算，
// 不包含撮合与风险语义。调用方必须保证参数合法（账户存在、金额非负）。
type Ledger struct {
	accounts map[string]*Account
}

func newLedger() *Ledger {
	return &Ledger{accounts: map[string]*Account{}}
}

func (l *Ledger) openAccount(id string, cash int64) error {
	if _, ok := l.accounts[id]; ok {
		return ErrAccountExists
	}
	l.accounts[id] = &Account{
		ID:          id,
		Cash:        cash,
		Positions:   map[string]int64{},
		ReservedPos: map[string]int64{},
	}
	return nil
}

func (l *Ledger) get(id string) (*Account, bool) {
	a, ok := l.accounts[id]
	return a, ok
}

// creditCash / creditPosition 入金/入券（Deposit 事件用）。
func (l *Ledger) creditCash(id string, amount int64) error {
	a, ok := l.accounts[id]
	if !ok {
		return errors.New("credit: account missing")
	}
	a.Cash += amount
	return nil
}

func (l *Ledger) creditPosition(id, symbol string, qty int64) error {
	a, ok := l.accounts[id]
	if !ok {
		return errors.New("credit: account missing")
	}
	a.Positions[symbol] += qty
	return nil
}

// reserveCash 冻结买单资金：可用现金减少、冻结现金增加。
func (l *Ledger) reserveCash(id string, amount int64) bool {
	a, ok := l.accounts[id]
	if !ok || amount < 0 || a.Cash < amount {
		return false
	}
	a.Cash -= amount
	a.ReservedCash += amount
	return true
}

// releaseCash 释放冻结资金。
func (l *Ledger) releaseCash(id string, amount int64) {
	a, ok := l.accounts[id]
	if !ok {
		return
	}
	a.ReservedCash -= amount
	a.Cash += amount
}

// canReserveCash / canReservePosition 仅做可行性判定，不改变任何状态。
func (l *Ledger) canReserveCash(id string, amount int64) bool {
	a, ok := l.accounts[id]
	return ok && amount >= 0 && a.Cash >= amount
}

func (l *Ledger) canReservePosition(id, symbol string, qty int64) bool {
	a, ok := l.accounts[id]
	return ok && qty >= 0 && a.Positions[symbol] >= qty
}

// reservePosition 冻结卖单持仓。
func (l *Ledger) reservePosition(id, symbol string, qty int64) bool {
	a, ok := l.accounts[id]
	if !ok || qty < 0 || a.Positions[symbol] < qty {
		return false
	}
	a.Positions[symbol] -= qty
	a.ReservedPos[symbol] += qty
	return true
}

// releasePosition 释放冻结持仓。
func (l *Ledger) releasePosition(id, symbol string, qty int64) {
	a, ok := l.accounts[id]
	if !ok {
		return
	}
	a.ReservedPos[symbol] -= qty
	a.Positions[symbol] += qty
}

// settleSide 对单个账户按成交方向即时交割：
// 买方现金减少、持仓增加；卖方持仓减少、现金增加。
// 调用方必须保证该账户的成交部分没有被预先冻结（主动市价单），
// 已冻结部分应先调用 release* 再调用本方法（见 applyReservedFill）。
func (l *Ledger) settleSide(a *Account, side Side, symbol string, price, qty int64) error {
	n, ok := notional(price, qty)
	if !ok {
		return errors.New("notional overflow")
	}
	if side == Buy {
		if a.Cash < n {
			return errors.New("settle: buyer cash insufficient (invariant)")
		}
		a.Cash -= n
		a.Positions[symbol] += qty
	} else {
		if a.Positions[symbol] < qty {
			return errors.New("settle: seller position insufficient (invariant)")
		}
		a.Positions[symbol] -= qty
		a.Cash += n
	}
	return nil
}

// applyReservedFill 已冻结资金/持仓的挂单方（含主动限价单）成交结算：
// 先按挂单价把成交对应的冻结部分释放回可用项，再按实际成交价交割。
// 买入成交价优于挂单价时，差额自然留在可用现金中。
func (l *Ledger) applyReservedFill(o *Order, fillPrice, qty int64) error {
	a, ok := l.accounts[o.Account]
	if !ok {
		return errors.New("settle: account missing")
	}
	rn, ok := notional(o.Price, qty) // 预留按挂单价计算
	if !ok {
		return errors.New("notional overflow (reserved)")
	}
	if o.Side == Buy {
		a.ReservedCash -= rn
		a.Cash += rn
	} else {
		a.ReservedPos[o.Symbol] -= qty
		a.Positions[o.Symbol] += qty
	}
	return l.settleSide(a, o.Side, o.Symbol, fillPrice, qty)
}
