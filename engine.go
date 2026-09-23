package matching

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Config 引擎配置。
type Config struct {
	JournalPath string          // 为空则纯内存、不持久化
	Limits      RiskLimits      // 风险限额
	STP         SelfTradePolicy // 自成交处置
}

// Engine 本地撮合与执行引擎。所有指令在同一临界区内按进入顺序分配全引擎
// 单调递增的 Seq 并串行撮合：多标的并发提交的结果与按同一顺序逐一执行
// 严格等价，且不存在数据竞争。
type Engine struct {
	mu      sync.Mutex
	ledger  *Ledger
	books   map[string]*Book
	orders  map[string]*Order // 含已终结订单（成交/撤销/拒绝），供查询与去重
	seq     int64
	journal *Journal
	limits  RiskLimits
	stp     SelfTradePolicy
}

// NewEngine 创建引擎；若 JournalPath 已存在流水则先截断半截尾记录并回放恢复。
// 路径为空时仅运行内存引擎。
func NewEngine(cfg Config) (*Engine, error) {
	e := newEmptyEngine(cfg)
	if cfg.JournalPath == "" {
		return e, nil
	}
	if _, err := RecoverJournal(cfg.JournalPath); err != nil {
		return nil, err
	}
	if err := replayInto(e, cfg.JournalPath); err != nil {
		return nil, err
	}
	j, err := openJournal(cfg.JournalPath)
	if err != nil {
		return nil, err
	}
	e.journal = j
	return e, nil
}

func newEmptyEngine(cfg Config) *Engine {
	stp := cfg.STP
	if stp == 0 {
		stp = STPCancelMaker
	}
	return &Engine{
		ledger: newLedger(),
		books:  map[string]*Book{},
		orders: map[string]*Order{},
		limits: cfg.Limits,
		stp:    stp,
	}
}

func (e *Engine) nextSeq() int64 {
	e.seq++
	return e.seq
}

// Close 关闭流水文件。
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.journal != nil {
		return e.journal.Close()
	}
	return nil
}

// OpenAccount 开立账户并存入初始现金（重复开立返回错误，不产生任何记录）。
func (e *Engine) OpenAccount(id string, initialCash int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if id == "" {
		return fmt.Errorf("open account: empty id")
	}
	if initialCash < 0 {
		return fmt.Errorf("open account: negative cash")
	}
	if _, ok := e.ledger.get(id); ok {
		return fmt.Errorf("open account %s: %w", id, ErrAccountExists)
	}
	seq := e.nextSeq()
	rec := &JournalRecord{
		Seq:  seq,
		Time: nowMillis(),
		Events: []Event{{
			Type:    EvAccountOpened,
			Seq:     seq,
			Time:    nowMillis(),
			Account: id,
			Cash:    initialCash,
		}},
	}
	if err := e.commit(rec); err != nil {
		e.seq--
		return err
	}
	return nil
}

// commit 先将记录持久化（fsync），成功后再施加到内存状态；
// 持久化失败时内存状态不变，调用方可在恢复后安全重试。
func (e *Engine) commit(rec *JournalRecord) error {
	if e.journal != nil {
		if err := e.journal.append(rec); err != nil {
			return err
		}
	}
	return e.applyRecord(rec)
}

// ---- 指令入口 ----

// Deposit 向账户入金（symbol 为空）或入券（symbol 非空），事件同样进入流水。
func (e *Engine) Deposit(account, symbol string, amount int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.ledger.get(account); !ok {
		return fmt.Errorf("deposit: account %s not found", account)
	}
	if amount <= 0 {
		return fmt.Errorf("deposit: amount %d must be positive", amount)
	}
	seq := e.nextSeq()
	now := nowMillis()
	rec := &JournalRecord{Seq: seq, Time: now, Events: []Event{{
		Type: EvDeposit, Seq: seq, Time: now, Account: account,
		Symbol: symbol, DepositAmount: amount,
	}}}
	if err := e.commit(rec); err != nil {
		e.seq--
		return err
	}
	return nil
}

// Submit 提交限价/市价订单。校验、风控、冻结、撮合是一次原子提交：
// 被拒绝的指令不会留下任何中间状态。
func (e *Engine) Submit(o SubmitOrder) SubmitResult {
	e.mu.Lock()
	defer e.mu.Unlock()

	if reason, detail := validateSubmit(o, e); reason != RejNone {
		return e.rejectCommand(o, reason, detail)
	}

	ord := &Order{
		ID:        o.ID,
		Account:   o.Account,
		Symbol:    o.Symbol,
		Side:      o.Side,
		Type:      o.Type,
		Price:     o.Price,
		OrigQty:   o.Qty,
		Status:    StatusNew,
		CreatedAt: nowMillis(),
	}

	if o.Type == Limit {
		return e.submitLimit(ord)
	}
	return e.submitMarket(ord)
}

func (e *Engine) submitLimit(ord *Order) SubmitResult {
	// 风控：提交前按全额增量判定。
	rv := e.riskView()
	if reason := checkRisk(e.limits, ord, rv); reason != RejNone {
		return e.rejectOrder(ord, reason, riskCheckDetail(reason, ord, -1, ord.Remaining(), -1))
	}
	plan := e.match(ord, false)
	if plan.rejectReason != RejNone {
		// 纯规划阶段无任何状态变更，直接拒绝即可。
		return e.rejectOrder(ord, plan.rejectReason, plan.rejectDetail)
	}
	// 全额冻结可行性判定；真正冻结在 applyRecord 中生效，与回放同一路径。
	if ok, reason, detail := e.canReserve(ord); !ok {
		return e.rejectOrder(ord, reason, detail)
	}

	finalStatus := StatusNew
	if ord.OrigQty == plan.totalFilled {
		finalStatus = StatusFilled
	} else if plan.totalFilled > 0 {
		finalStatus = StatusPartiallyFilled
	}

	seq := e.nextSeq()
	now := nowMillis()
	ord.Seq = seq
	ord.Status = finalStatus
	// 接受事件保持 FilledQty=0 以便按原始全额冻结；立即成交量由事件字段携带。
	events := make([]Event, 0, 1+len(plan.fills))
	events = append(events, Event{
		Type: EvOrderAccepted, Seq: seq, Time: now, Order: ord,
		FilledAtAccept: plan.totalFilled,
	})
	for _, f := range plan.fills {
		if f.stpCancel {
			ev := f.makerEv
			ev.Seq = seq
			ev.Time = now
			events = append(events, ev)
			continue
		}
		// 一笔成交仅对应一个事件，applyFill 内部同时结算买卖双方，
		// 避免重复事件造成重复成交。
		events = append(events, Event{
			Type: EvOrderFilled, Seq: seq, Time: now, Symbol: ord.Symbol,
			TakerID: f.takerID, MakerID: f.makerID, Price: f.price, Qty: f.qty,
		})
	}
	if err := e.commit(&JournalRecord{Seq: seq, Time: now, Events: events}); err != nil {
		e.seq--
		return SubmitResult{OrderID: ord.ID, Seq: seq, Reason: RejJournalIO, Err: err}
	}
	ord.FilledQty = plan.totalFilled
	return e.makeResult(ord, seq, plan)
}

func (e *Engine) submitMarket(ord *Order) SubmitResult {
	// 市价单不挂簿，需先具备即时可承受能力，否则给出确定且可区分的原因。
	acct, _ := e.ledger.get(ord.Account)
	if ord.Side == Buy {
		if acct.Cash <= 0 {
			return e.rejectOrder(ord, RejInsufficientFunds,
				fmt.Sprintf("market buy %s: account %s has no available cash", ord.ID, ord.Account))
		}
	} else {
		if acct.Positions[ord.Symbol] <= 0 {
			return e.rejectOrder(ord, RejInsufficientPosition,
				fmt.Sprintf("market sell %s: account %s has no free position in %s",
					ord.ID, ord.Account, ord.Symbol))
		}
	}
	plan := e.match(ord, true)
	if plan.rejectReason != RejNone {
		return e.rejectOrder(ord, plan.rejectReason, plan.rejectDetail)
	}
	if plan.totalFilled == 0 {
		// 市价单无法成交：无流动性 / 资金或持仓为 0，明确拒绝且不改状态。
		reason := plan.stopReason
		if reason == RejNone {
			reason = RejNoLiquidity
		}
		return e.rejectOrder(ord, reason,
			fmt.Sprintf("market order %s not fillable: %s", ord.ID, reason))
	}

	finalStatus := StatusFilled
	ioc := false
	if plan.totalFilled < ord.OrigQty {
		finalStatus = StatusCancelled // 市价 IOC：剩余立即撤销
		ioc = true
	}

	seq := e.nextSeq()
	now := nowMillis()
	ord.Seq = seq
	ord.Status = finalStatus
	events := make([]Event, 0, 2+len(plan.fills))
	events = append(events, Event{
		Type: EvOrderAccepted, Seq: seq, Time: now, Order: ord,
		FilledAtAccept: plan.totalFilled,
	})
	for _, f := range plan.fills {
		if f.stpCancel {
			ev := f.makerEv
			ev.Seq = seq
			ev.Time = now
			events = append(events, ev)
			continue
		}
		// 一笔成交仅对应一个事件，applyFill 内部同时结算买卖双方，
		// 避免重复事件造成重复成交。
		events = append(events, Event{
			Type: EvOrderFilled, Seq: seq, Time: now, Symbol: ord.Symbol,
			TakerID: f.takerID, MakerID: f.makerID, Price: f.price, Qty: f.qty,
		})
	}
	if ioc {
		events = append(events, Event{
			Type: EvOrderCancelled, Seq: seq, Time: now, Symbol: ord.Symbol,
			Order: ord, Reason: RejNoLiquidity,
			Detail: fmt.Sprintf("market IOC residual %d cancelled", ord.OrigQty-plan.totalFilled),
		})
	}
	if err := e.commit(&JournalRecord{Seq: seq, Time: now, Events: events}); err != nil {
		e.seq--
		return SubmitResult{OrderID: ord.ID, Seq: seq, Reason: RejJournalIO, Err: err}
	}
	ord.FilledQty = plan.totalFilled
	return e.makeResult(ord, seq, plan)
}

// Cancel 撤销未成交部分，释放全部冻结资金/持仓。
func (e *Engine) Cancel(c CancelOrder) SubmitResult {
	e.mu.Lock()
	defer e.mu.Unlock()

	if c.ID == "" {
		return e.rejectRef("cancel", "", "", RejInvalidParam, "cancel: empty order id")
	}
	ord, ok := e.orders[c.ID]
	if !ok {
		return e.rejectRef("cancel", c.ID, "", RejUnknownOrder, "cancel: order not found")
	}
	if ord.Status != StatusNew && ord.Status != StatusPartiallyFilled {
		return e.rejectRef("cancel", c.ID, ord.Symbol, RejOrderClosed,
			fmt.Sprintf("cancel: order already %s", ord.Status))
	}

	seq := e.nextSeq()
	now := nowMillis()
	cancelCopy := *ord
	cancelCopy.Status = StatusCancelled
	rec := &JournalRecord{Seq: seq, Time: now, Events: []Event{{
		Type: EvOrderCancelled, Seq: seq, Time: now, Symbol: ord.Symbol,
		Order: &cancelCopy, Reason: RejNone, Detail: "user cancel",
	}}}
	if err := e.commit(rec); err != nil {
		e.seq--
		return SubmitResult{OrderID: c.ID, Reason: RejJournalIO, Err: err}
	}
	ord.Status = StatusCancelled
	return SubmitResult{OrderID: c.ID, Seq: seq, Status: StatusCancelled, Accepted: true}
}

// rejectRef 持久化一条针对已知/未知订单的拒绝记录（撤单、改单被拒时使用），
// 使其带 Seq、可回放、可复现。
func (e *Engine) rejectRef(kind, id, symbol string, reason RejectReason, detail string) SubmitResult {
	seq := e.nextSeq()
	now := nowMillis()
	ev := Event{Type: EvOrderRejected, Seq: seq, Time: now, Symbol: symbol,
		Reason: reason, Detail: kind + ": " + detail}
	if ord, ok := e.orders[id]; ok {
		ev.Order = ord
	} else {
		ev.Order = &Order{ID: id, Symbol: symbol, Status: StatusRejected}
	}
	rec := &JournalRecord{Seq: seq, Time: now, Events: []Event{ev}}
	if err := e.commit(rec); err != nil {
		e.seq--
		return SubmitResult{OrderID: id, Reason: RejJournalIO, Err: err}
	}
	return SubmitResult{OrderID: id, Seq: seq, Status: StatusRejected,
		Reason: reason, Detail: detail, Accepted: false}
}

// Amend 同价改量：保留队列优先级。缩量立即释放多余冻结；
// 增量需通过资金/持仓与风险校验，任一不满足则整单拒绝、状态不变。
func (e *Engine) Amend(a AmendOrder) SubmitResult {
	e.mu.Lock()
	defer e.mu.Unlock()

	if a.ID == "" {
		return e.rejectRef("amend", "", "", RejInvalidParam, "amend: empty order id")
	}
	ord, ok := e.orders[a.ID]
	if !ok {
		return e.rejectRef("amend", a.ID, "", RejUnknownOrder, "amend: order not found")
	}
	if ord.Status != StatusNew && ord.Status != StatusPartiallyFilled {
		return e.rejectRef("amend", a.ID, ord.Symbol, RejOrderClosed,
			fmt.Sprintf("amend: order already %s", ord.Status))
	}
	if ord.Type == Market {
		return e.rejectRef("amend", a.ID, ord.Symbol, RejOrderClosed,
			"amend: market order is IOC and cannot rest")
	}
	if a.NewPrice != 0 && a.NewPrice != ord.Price {
		return e.rejectRef("amend", a.ID, ord.Symbol, RejAmendPriceChange,
			fmt.Sprintf("amend: price change %d->%d forbidden (would lose time priority)",
				ord.Price, a.NewPrice))
	}
	if a.NewQty <= 0 || a.NewQty <= ord.FilledQty {
		return e.rejectRef("amend", a.ID, ord.Symbol, RejAmendInvalidQty,
			fmt.Sprintf("amend: new qty %d must exceed filled qty %d", a.NewQty, ord.FilledQty))
	}

	oldQty := ord.OrigQty
	delta := a.NewQty - oldQty
	if delta > 0 {
		// 增量仅做可行性判定；真正冻结在 applyRecord 中随事件一起生效，
		// 保证实时执行与事件回放共用同一条状态迁移路径。
		probe := *ord
		probe.OrigQty = a.NewQty
		if reason := checkRisk(e.limits, &probe, e.riskView()); reason != RejNone {
			return e.rejectRef("amend", a.ID, ord.Symbol, reason,
				riskCheckDetail(reason, &probe, -1, delta, -1))
		}
		if ord.Side == Buy {
			add, _ := notional(ord.Price, delta)
			if !e.ledger.canReserveCash(ord.Account, add) {
				return e.rejectRef("amend", a.ID, ord.Symbol, RejInsufficientFunds,
					fmt.Sprintf("amend: need %d more cash for %s", add, ord.ID))
			}
		} else {
			if !e.ledger.canReservePosition(ord.Account, ord.Symbol, delta) {
				return e.rejectRef("amend", a.ID, ord.Symbol, RejInsufficientPosition,
					fmt.Sprintf("amend: need %d more position for %s", delta, ord.ID))
			}
		}
	}

	newStatus := StatusNew
	if ord.FilledQty > 0 {
		newStatus = StatusPartiallyFilled
	}
	if a.NewQty == ord.FilledQty {
		newStatus = StatusFilled
	}

	// 事件携带订单副本，不预先改写活动订单；只有 commit 成功后才写回，
	// 确保流水持久化失败时订单状态零改动。
	ordCopy := *ord
	ordCopy.OrigQty = a.NewQty
	ordCopy.Status = newStatus

	seq := e.nextSeq()
	now := nowMillis()
	rec := &JournalRecord{Seq: seq, Time: now, Events: []Event{{
		Type: EvOrderAmended, Seq: seq, Time: now, Symbol: ord.Symbol,
		Order: &ordCopy, OldQty: oldQty, NewQty: a.NewQty,
	}}}
	if err := e.commit(rec); err != nil {
		e.seq--
		return SubmitResult{OrderID: a.ID, Reason: RejJournalIO, Err: err}
	}
	ord.OrigQty = a.NewQty
	ord.Status = newStatus
	return SubmitResult{OrderID: a.ID, Seq: seq, Status: newStatus, Accepted: true}
}

// ---- 校验、冻结、风控视图 ----

func validateSubmit(o SubmitOrder, e *Engine) (RejectReason, string) {
	if o.ID == "" {
		return RejInvalidParam, "submit: empty order id"
	}
	if o.Account == "" || o.Symbol == "" {
		return RejInvalidParam, "submit: empty account or symbol"
	}
	if _, ok := e.ledger.get(o.Account); !ok {
		return RejUnknownAccount, fmt.Sprintf("submit: account %s not found", o.Account)
	}
	if o.Side != Buy && o.Side != Sell {
		return RejInvalidParam, "submit: invalid side"
	}
	if o.Type != Limit && o.Type != Market {
		return RejInvalidParam, "submit: invalid order type"
	}
	if o.Qty <= 0 {
		return RejInvalidQuantity, fmt.Sprintf("submit: qty %d must be positive", o.Qty)
	}
	if _, dup := e.orders[o.ID]; dup {
		return RejDuplicateOrderID, fmt.Sprintf("submit: order id %s already exists", o.ID)
	}
	if o.Type == Limit && o.Price <= 0 {
		return RejInvalidPrice, fmt.Sprintf("submit: limit price %d must be positive", o.Price)
	}
	if o.Type == Market && o.Price != 0 {
		return RejInvalidPrice, "submit: market order price must be zero"
	}
	if n, ok := notional(max64(o.Price, 1), o.Qty); !ok {
		_ = n
		return RejInvalidQuantity, "submit: notional overflow"
	}
	return RejNone, ""
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// canReserve 对限价单做全额冻结可行性判定，不改变任何状态，返回失败原因。
func (e *Engine) canReserve(o *Order) (bool, RejectReason, string) {
	if o.Side == Buy {
		need, _ := notional(o.Price, o.OrigQty)
		if !e.ledger.canReserveCash(o.Account, need) {
			return false, RejInsufficientFunds,
				fmt.Sprintf("submit: account %s cash < %d for buy %s", o.Account, need, o.ID)
		}
		return true, RejNone, ""
	}
	if !e.ledger.canReservePosition(o.Account, o.Symbol, o.OrigQty) {
		return false, RejInsufficientPosition,
			fmt.Sprintf("submit: account %s position %s < %d for sell %s",
				o.Account, o.Symbol, o.OrigQty, o.ID)
	}
	return true, RejNone, ""
}

func (e *Engine) riskView() riskView {
	return riskView{
		accountBuyNotional: e.accountBuyNotional,
		symbolPosition:     e.symbolNetPosition,
		globalExposure:     e.globalExposure,
	}
}

// accountBuyNotional 账户当前在簿买单剩余名义金额（与其冻结买单现金一致）。
func (e *Engine) accountBuyNotional(account string) int64 {
	var total int64
	for _, b := range e.books {
		for _, lvl := range b.bids {
			for el := lvl.queue.Front(); el != nil; el = el.Next() {
				o := el.Value.(*Order)
				if o.Account == account {
					if n, ok := notional(o.Price, o.Remaining()); ok {
						total += n
					}
				}
			}
		}
	}
	return total
}

// symbolNetPosition 账户在某标的的净持仓 = 可用持仓 - 卖单冻结持仓。
func (e *Engine) symbolNetPosition(account, symbol string) int64 {
	a, ok := e.ledger.get(account)
	if !ok {
		return 0
	}
	return a.Positions[symbol] - a.ReservedPos[symbol]
}

func (e *Engine) globalExposure() int64 {
	var total int64
	for _, b := range e.books {
		total += b.openExposure()
	}
	return total
}

// ---- 拒绝记录 ----

// rejectCommand 为原始指令构造并落盘一条拒绝记录（指令尚未构造成订单时）。
func (e *Engine) rejectCommand(o SubmitOrder, reason RejectReason, detail string) SubmitResult {
	ord := &Order{
		ID: o.ID, Account: o.Account, Symbol: o.Symbol, Side: o.Side,
		Type: o.Type, Price: o.Price, OrigQty: o.Qty,
		Status: StatusRejected, CreatedAt: nowMillis(),
	}
	return e.persistReject(ord, reason, detail)
}

func (e *Engine) rejectOrder(ord *Order, reason RejectReason, detail string) SubmitResult {
	ord.Status = StatusRejected
	ord.FilledQty = 0
	return e.persistReject(ord, reason, detail)
}

func (e *Engine) persistReject(ord *Order, reason RejectReason, detail string) SubmitResult {
	seq := e.nextSeq()
	now := nowMillis()
	rec := &JournalRecord{Seq: seq, Time: now, Events: []Event{{
		Type: EvOrderRejected, Seq: seq, Time: now, Symbol: ord.Symbol,
		Order: ord, Reason: reason, Detail: detail,
	}}}
	if err := e.commit(rec); err != nil {
		e.seq--
		return SubmitResult{OrderID: ord.ID, Seq: seq, Reason: RejJournalIO, Err: err}
	}
	return SubmitResult{OrderID: ord.ID, Seq: seq, Status: StatusRejected,
		Reason: reason, Detail: detail, Accepted: false}
}

func (e *Engine) makeResult(ord *Order, seq int64, plan *matchPlan) SubmitResult {
	fills := make([]FillView, 0, len(plan.fills))
	for _, f := range plan.fills {
		fills = append(fills, FillView{
			Seq: seq, Time: ord.CreatedAt, Symbol: ord.Symbol,
			TakerID: f.takerID, MakerID: f.makerID, TakerSide: ord.Side,
			Price: f.price, Qty: f.qty,
		})
	}
	return SubmitResult{OrderID: ord.ID, Seq: seq, Status: ord.Status,
		Fills: fills, Accepted: true}
}

// ---- 查询 ----

func (e *Engine) GetOrder(id string) (*Order, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	o, ok := e.orders[id]
	if !ok {
		return nil, false
	}
	cp := *o
	return &cp, true
}

func (e *Engine) GetAccount(id string) (AccountSnapshot, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a, ok := e.ledger.get(id)
	if !ok {
		return AccountSnapshot{}, false
	}
	return snapshotAccount(a), true
}

func snapshotAccount(a *Account) AccountSnapshot {
	pos := make(map[string]int64, len(a.Positions))
	for k, v := range a.Positions {
		pos[k] = v
	}
	res := make(map[string]int64, len(a.ReservedPos))
	for k, v := range a.ReservedPos {
		res[k] = v
	}
	return AccountSnapshot{
		ID: a.ID, Cash: a.Cash, ReservedCash: a.ReservedCash,
		Positions: pos, ReservedPos: res,
	}
}

func (e *Engine) BookSnapshot(symbol string, levels int) BookSnapshotView {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.bookSnapshotLocked(symbol, levels)
}

func (e *Engine) bookSnapshotLocked(symbol string, levels int) BookSnapshotView {
	b, ok := e.books[symbol]
	view := BookSnapshotView{Symbol: symbol, Bids: []BookLevelView{}, Asks: []BookLevelView{}}
	if !ok {
		return view
	}
	add := func(prices []int64, m map[int64]*priceLevel) []BookLevelView {
		out := make([]BookLevelView, 0, len(prices))
		for _, p := range prices {
			var q int64
			for el := m[p].queue.Front(); el != nil; el = el.Next() {
				q += el.Value.(*Order).Remaining()
			}
			out = append(out, BookLevelView{Price: p, Qty: q})
			if levels > 0 && len(out) >= levels {
				break
			}
		}
		return out
	}
	view.Bids = add(b.bidPx, b.bids)
	view.Asks = add(b.askPx, b.asks)
	return view
}

// sortedSymbols 返回标的有序列表，保证快照/日志顺序稳定。
func (e *Engine) sortedSymbols() []string {
	out := make([]string, 0, len(e.books))
	for s := range e.books {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func nowMillis() int64 { return time.Now().UnixMilli() }
