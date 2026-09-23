package matching

// BookLevelView 单档行情。
type BookLevelView struct {
	Price int64 `json:"price"`
	Qty   int64 `json:"qty"`
}

// BookSnapshotView 订单簿快照。
type BookSnapshotView struct {
	Symbol string          `json:"symbol"`
	Bids   []BookLevelView `json:"bids"`
	Asks   []BookLevelView `json:"asks"`
}

// EngineSnapshot 引擎全量状态快照（用于对账与测试断言）。
type EngineSnapshot struct {
	Seq      int64                       `json:"seq"`
	Accounts map[string]AccountSnapshot  `json:"accounts"`
	Orders   map[string]Order            `json:"orders"`
	Books    map[string]BookSnapshotView `json:"books"`
}

// Snapshot 返回调用时刻的全量深拷贝快照。
func (e *Engine) Snapshot() EngineSnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()

	accts := make(map[string]AccountSnapshot, len(e.ledger.accounts))
	for id, a := range e.ledger.accounts {
		accts[id] = snapshotAccount(a)
	}
	orders := make(map[string]Order, len(e.orders))
	for id, o := range e.orders {
		orders[id] = *o
	}
	books := make(map[string]BookSnapshotView, len(e.books))
	for _, s := range e.sortedSymbols() {
		books[s] = e.bookSnapshotLocked(s, 0)
	}
	return EngineSnapshot{Seq: e.seq, Accounts: accts, Orders: orders, Books: books}
}
