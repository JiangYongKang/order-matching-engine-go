package engine

import "time"

// staticValidate checks instruction fields independent of engine state.
// Precedence: identity fields first, then side/type, then quantity, price.
func staticValidate(req OrderRequest) *RejectError {
	if req.ID == "" {
		return rejectf(RejectEmptyOrderID, "order id must not be empty")
	}
	if req.Account == "" {
		return rejectf(RejectUnknownAccount, "account must not be empty")
	}
	if req.Symbol == "" {
		return rejectf(RejectUnknownSymbol, "symbol must not be empty")
	}
	if req.Side != SideBuy && req.Side != SideSell {
		return rejectf(RejectInvalidSide, "side %d is invalid", req.Side)
	}
	if req.Type != OrderTypeLimit && req.Type != OrderTypeMarket {
		return rejectf(RejectInvalidOrderType, "order type %d is invalid", req.Type)
	}
	if req.Qty <= 0 {
		return rejectf(RejectInvalidQty, "quantity %d must be positive", req.Qty)
	}
	if req.Type == OrderTypeLimit && req.Price <= 0 {
		return rejectf(RejectInvalidPrice, "limit price %d must be positive", req.Price)
	}
	if req.Type == OrderTypeMarket && req.Price != 0 {
		return rejectf(RejectInvalidPrice, "market order must not carry a price (got %d)", req.Price)
	}
	return nil
}

// settlementPlan is read-only book analysis for a submit command, produced by
// the symbol shard. All ledger-affecting decisions happen later under the
// global lock in commitSubmit.
type settlementPlan struct {
	req          OrderRequest
	staticReject *RejectError
	candidates   []FillCandidate // price-time priority walk across the book
	selfTrade    bool
}

// buildSubmitPlan performs shard-local book analysis (the shard is the single
// writer of its book, so this needs no additional synchronization).
func (e *Engine) buildSubmitPlan(s *shard, req OrderRequest) *settlementPlan {
	p := &settlementPlan{req: req}
	if rj := staticValidate(req); rj != nil {
		p.staticReject = rj
		return p
	}
	hasLimit := req.Type == OrderTypeLimit
	p.candidates = s.book.MatchPlan(req.Side, req.Price, req.Qty, hasLimit)
	for _, c := range p.candidates {
		if c.Order.Account == req.Account {
			// Conservative self-trade prevention (cancel-newest): the whole
			// incoming order is rejected. Resting orders keep priority and
			// no trade is produced, so there is never a partial self-fill.
			p.selfTrade = true
			break
		}
	}
	return p
}

type plannedTrade struct {
	maker *Order
	price int64
	qty   int64
}

// planTrades turns the candidate walk into concrete fills. Deterministic:
// candidate order is fixed price-time priority from the shard book.
func planTrades(p *settlementPlan) []plannedTrade {
	remaining := p.req.Qty
	var out []plannedTrade
	for _, c := range p.candidates {
		if remaining <= 0 {
			break
		}
		take := c.Order.OpenQty
		if take > remaining {
			take = remaining
		}
		if take <= 0 {
			continue
		}
		out = append(out, plannedTrade{maker: c.Order, price: c.Price, qty: take})
		remaining -= take
	}
	return out
}

// commitSubmit atomically validates, journals and settles a submit command.
func (e *Engine) commitSubmit(p *settlementPlan) (*SubmitResult, error) {
	req := p.req
	res := &SubmitResult{}

	e.mu.Lock()
	defer e.mu.Unlock()
	v := e.view
	now := e.clock()

	if p.staticReject != nil {
		return e.finishSubmitRejectLocked(res, req, p.staticReject, now)
	}
	if _, dup := v.orders[req.ID]; dup {
		return e.finishSubmitRejectLocked(res, req,
			rejectf(RejectDuplicateOrderID, "order id %q already exists", req.ID), now)
	}
	if _, dup := v.rejected[req.ID]; dup {
		return e.finishSubmitRejectLocked(res, req,
			rejectf(RejectDuplicateOrderID, "order id %q was previously rejected", req.ID), now)
	}
	if !v.ledger.HasAccount(req.Account) {
		return e.finishSubmitRejectLocked(res, req,
			rejectf(RejectUnknownAccount, "account %q is not registered", req.Account), now)
	}

	trades := planTrades(p)
	fillQty := int64(0)
	var fillCost int64
	for _, t := range trades {
		fillQty += t.qty
		fillCost += t.price * t.qty
	}
	restQty := req.Qty - fillQty
	isLimit := req.Type == OrderTypeLimit

	if req.Type == OrderTypeMarket && fillQty == 0 {
		return e.finishSubmitRejectLocked(res, req, rejectf(RejectNoLiquidity,
			"market order %s %s qty=%d found no opposing liquidity", req.ID, req.Side, req.Qty), now)
	}
	if p.selfTrade {
		return e.finishSubmitRejectLocked(res, req, rejectf(RejectSelfTrade,
			"order %s from account %s would trade against the same account", req.ID, req.Account), now)
	}

	acct := v.ledger.accounts[req.Account]
	switch {
	case req.Side == SideBuy && isLimit:
		need := fillCost + req.Price*restQty
		if acct.availCash() < need {
			return e.finishSubmitRejectLocked(res, req, rejectf(RejectInsufficientFunds,
				"buy %s needs cash %d (fill cost %d + reserve %d) but available is %d",
				req.Symbol, need, fillCost, req.Price*restQty, acct.availCash()), now)
		}
	case req.Side == SideSell && isLimit:
		if acct.availShares(req.Symbol) < req.Qty {
			return e.finishSubmitRejectLocked(res, req, rejectf(RejectInsufficientShares,
				"sell %s needs %d shares but available is %d",
				req.Symbol, req.Qty, acct.availShares(req.Symbol)), now)
		}
	case req.Side == SideBuy: // market buy: cost accumulated only at matched prices
		if acct.availCash() < fillCost {
			return e.finishSubmitRejectLocked(res, req, rejectf(RejectInsufficientFunds,
				"market buy %s needs cash %d but available is %d",
				req.Symbol, fillCost, acct.availCash()), now)
		}
	case req.Side == SideSell: // market sell
		if acct.availShares(req.Symbol) < fillQty {
			return e.finishSubmitRejectLocked(res, req, rejectf(RejectInsufficientShares,
				"market sell %s needs %d shares but available is %d",
				req.Symbol, fillQty, acct.availShares(req.Symbol)), now)
		}
	}

	// Risk constraints. Worst-case valuation: full original qty at limit
	// price, or at the first (best) matched price for market orders.
	valuationPrice := req.Price
	if !isLimit && len(trades) > 0 {
		valuationPrice = trades[0].price
	}
	riskRestQty := int64(0)
	if isLimit {
		riskRestQty = restQty
	}
	if rj := e.risk.Check(riskInputs{
		account: req.Account, symbol: req.Symbol, side: req.Side,
		price: valuationPrice, qty: req.Qty, restQty: riskRestQty,
		ledger: v.ledger,
	}); rj != nil {
		return e.finishSubmitRejectLocked(res, req, rj, now)
	}

	// Build the event group: Accepted is allocated first (its seq doubles as
	// time priority), followed by one Trade per fill in execution order.
	prioritySeq := e.allocSeqLocked()
	taker := &Order{
		ID: req.ID, Account: req.Account, Symbol: req.Symbol, Side: req.Side,
		Type: req.Type, Price: req.Price, Qty: req.Qty,
		OpenQty: restQty, FilledQty: 0, PrioritySeq: prioritySeq,
		CreatedAt: now,
	}
	if !isLimit {
		taker.OpenQty = 0 // market leftovers are IOC-canceled, never rest
	}
	accEv := &Event{
		Seq: prioritySeq, Type: EventAccepted, Time: now,
		OrderID: req.ID, Order: taker, Account: req.Account, Symbol: req.Symbol,
		Side: req.Side, OType: req.Type, Price: req.Price, Qty: req.Qty,
	}
	if isLimit && restQty > 0 {
		if req.Side == SideBuy {
			accEv.ReservedCash = req.Price * restQty
		} else {
			accEv.ReservedShares = restQty
		}
	}
	events := []*Event{accEv}
	resTrades := make([]*Trade, 0, len(trades))
	for _, t := range trades {
		prioritySeq++
		tr := &Trade{
			Seq: prioritySeq, Symbol: req.Symbol, Price: t.price, Qty: t.qty,
			MakerOrderID: t.maker.ID, MakerAccount: t.maker.Account,
			TakerOrderID: req.ID, TakerAccount: req.Account, Time: now,
		}
		events = append(events, &Event{Seq: prioritySeq, Type: EventTrade, Time: now, OrderID: req.ID, Trade: tr})
		resTrades = append(resTrades, tr)
	}
	e.seekSeqLocked(prioritySeq)
	if err := e.persistAndApplyLocked(events); err != nil {
		return nil, err
	}

	res.Order = v.orders[req.ID]
	res.Trades = resTrades
	res.Seq = prioritySeq
	res.Resting = isLimit && restQty > 0
	switch {
	case res.Resting:
		res.Status = res.Order.Status()
	case fillQty == req.Qty:
		res.Status = StatusFilled
	default:
		res.Status = StatusCanceled // market: partially filled, remainder IOC-canceled
	}
	return res, nil
}

func (e *Engine) finishSubmitRejectLocked(res *SubmitResult, req OrderRequest, rj *RejectError, now time.Time) (*SubmitResult, error) {
	ev := &Event{
		Seq: e.allocSeqLocked(), Type: EventRejected, Time: now,
		OrderID: req.ID, Account: req.Account, Symbol: req.Symbol,
		Side: req.Side, OType: req.Type, Price: req.Price, Qty: req.Qty,
		Reason: string(rj.Reason),
	}
	if err := e.persistAndApplyLocked([]*Event{ev}); err != nil {
		return nil, err
	}
	res.Reject = rj
	res.Status = StatusRejected
	res.Seq = ev.Seq
	return res, nil
}

// commitCancel handles a cancel instruction atomically.
func (e *Engine) commitCancel(orderID string) (*CancelResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v := e.view
	rj := e.checkInstructionTargetLocked(orderID)
	if rj != nil {
		return &CancelResult{OrderID: orderID, Reject: rj}, nil
	}
	o := v.orders[orderID]
	ev := &Event{
		Seq: e.allocSeqLocked(), Type: EventCanceled, Time: e.clock(),
		OrderID: orderID, Account: o.Account, Symbol: o.Symbol,
	}
	if err := e.persistAndApplyLocked([]*Event{ev}); err != nil {
		return nil, err
	}
	return &CancelResult{OrderID: orderID, Seq: ev.Seq}, nil
}

// commitReplace handles an amend instruction atomically.
//
// Same price: priority at the level is preserved. A shrink releases the
// difference; a grow requires extra funds/holdings and passes risk again.
// Different price: the order is withdrawn (reservation released) and then
// matched/rested exactly like a new submit, but keeps the same order ID.
func (e *Engine) commitReplace(s *shard, orderID string, newPrice, newQty int64) (*ReplaceResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v := e.view
	now := e.clock()
	result := &ReplaceResult{}

	if rj := e.checkInstructionTargetLocked(orderID); rj != nil {
		result.Reject = rj
		return result, nil
	}
	old := v.orders[orderID]
	if old.Type == OrderTypeLimit && newPrice <= 0 {
		result.Reject = rejectf(RejectInvalidReplace, "replacement price %d must be positive", newPrice)
		return result, nil
	}
	if newQty <= 0 {
		result.Reject = rejectf(RejectInvalidReplace, "replacement qty %d must be positive", newQty)
		return result, nil
	}

	// ---- same-price amend: keep position in the level ----
	if old.Type == OrderTypeLimit && newPrice == old.Price {
		acct := v.ledger.accounts[old.Account]
		if newQty < old.OpenQty {
			// shrink: no risk impact, release freed reservation
		} else if newQty > old.OpenQty {
			extra := newQty - old.OpenQty
			if old.Side == SideBuy && acct.availCash() < newPrice*extra {
				result.Reject = rejectf(RejectInsufficientFunds,
					"replace grow %s needs extra cash %d but available is %d",
					old.Symbol, newPrice*extra, acct.availCash())
				return result, nil
			}
			if old.Side == SideSell && acct.availShares(old.Symbol) < extra {
				result.Reject = rejectf(RejectInsufficientShares,
					"replace grow %s needs %d extra shares but available is %d",
					old.Symbol, extra, acct.availShares(old.Symbol))
				return result, nil
			}
			if rj := e.risk.Check(riskInputs{
				account: old.Account, symbol: old.Symbol, side: old.Side,
				price: newPrice, qty: newQty, restQty: newQty,
				ledger: v.ledger, replacingOrder: old,
			}); rj != nil {
				result.Reject = rj
				return result, nil
			}
		}
		ev := &Event{
			Seq: e.allocSeqLocked(), Type: EventReplaced, Time: now,
			OrderID: orderID, Account: old.Account, Symbol: old.Symbol,
			Side: old.Side, OType: old.Type, Price: newPrice, Qty: newQty,
			KeptPriority: true,
		}
		if err := e.persistAndApplyLocked([]*Event{ev}); err != nil {
			return nil, err
		}
		result.Order = v.orders[orderID]
		result.Seq = ev.Seq
		result.KeptPriority = true
		return result, nil
	}
	// ---- price changed (or market, which cannot be amended) ----
	if old.Type == OrderTypeMarket {
		result.Reject = rejectf(RejectInvalidReplace, "market order %s cannot be amended", orderID)
		return result, nil
	}

	// Analyze matching against the book *as if* the old order were removed.
	candidates := s.book.MatchPlan(old.Side, newPrice, newQty, true)
	var selfAcct string
	selfTrade := false
	for _, c := range candidates {
		if c.Order.ID == old.ID {
			continue
		}
		if c.Order.Account == old.Account {
			selfTrade = true
			selfAcct = old.Account
			break
		}
	}
	if selfTrade {
		result.Reject = rejectf(RejectSelfTrade,
			"replace of %s for account %s would cross the same account", orderID, selfAcct)
		return result, nil
	}
	// Rebuild trades using only live candidates (excluding the old order).
	remaining := newQty
	var trades []plannedTrade
	for _, c := range candidates {
		if c.Order.ID == old.ID || remaining <= 0 {
			continue
		}
		take := c.Order.OpenQty
		if take > remaining {
			take = remaining
		}
		if take > 0 {
			trades = append(trades, plannedTrade{maker: c.Order, price: c.Price, qty: take})
			remaining -= take
		}
	}
	restQty := remaining
	fillCost := int64(0)
	for _, t := range trades {
		fillCost += t.price * t.qty
	}
	acct := v.ledger.accounts[old.Account]
	// Funds available after the old reservation is conceptually released.
	if old.Side == SideBuy {
		freed := old.Price * old.OpenQty
		if acct.availCash()+freed < fillCost+newPrice*restQty {
			result.Reject = rejectf(RejectInsufficientFunds,
				"replace %s needs cash %d (fill %d + reserve %d), freed %d, available %d",
				old.Symbol, fillCost+newPrice*restQty, fillCost, newPrice*restQty,
				freed, acct.availCash())
			return result, nil
		}
	} else {
		// old shares were reserved; total requirement must be coverable
		if acct.positions[old.Symbol] < newQty {
			result.Reject = rejectf(RejectInsufficientShares,
				"replace %s needs %d shares but position is %d",
				old.Symbol, newQty, acct.positions[old.Symbol])
			return result, nil
		}
	}
	if rj := e.risk.Check(riskInputs{
		account: old.Account, symbol: old.Symbol, side: old.Side,
		price: newPrice, qty: newQty, restQty: restQty,
		ledger: v.ledger, replacingOrder: old,
	}); rj != nil {
		result.Reject = rj
		return result, nil
	}

	// Build events: Replaced(false) (withdraw old), then Accepted (same ID,
	// fresh priority), then Trades. Sequences are contiguous: the counter is
	// advanced along the group and snapped to its final value at the end.
	repSeq := e.allocSeqLocked()
	lastSeq := repSeq
	prioritySeq := e.allocSeqLocked()
	lastSeq = prioritySeq
	newOrder := &Order{
		ID: old.ID, Account: old.Account, Symbol: old.Symbol, Side: old.Side,
		Type: OrderTypeLimit, Price: newPrice, Qty: newQty,
		OpenQty: restQty, FilledQty: 0, PrioritySeq: prioritySeq, CreatedAt: now,
	}
	repEv := &Event{
		Seq: repSeq, Type: EventReplaced, Time: now,
		OrderID: orderID, Account: old.Account, Symbol: old.Symbol,
		Side: old.Side, OType: old.Type, Price: newPrice, Qty: newQty,
		KeptPriority: false, Order: newOrder,
	}
	accEv := &Event{
		Seq: prioritySeq, Type: EventAccepted, Time: now,
		OrderID: orderID, Order: newOrder, Account: old.Account, Symbol: old.Symbol,
		Side: old.Side, OType: OrderTypeLimit, Price: newPrice, Qty: newQty,
	}
	if restQty > 0 {
		if old.Side == SideBuy {
			accEv.ReservedCash = newPrice * restQty
		} else {
			accEv.ReservedShares = restQty
		}
	}
	events := []*Event{repEv, accEv}
	resTrades := make([]*Trade, 0, len(trades))
	for _, t := range trades {
		lastSeq++
		tr := &Trade{
			Seq: lastSeq, Symbol: old.Symbol, Price: t.price, Qty: t.qty,
			MakerOrderID: t.maker.ID, MakerAccount: t.maker.Account,
			TakerOrderID: orderID, TakerAccount: old.Account, Time: now,
		}
		events = append(events, &Event{Seq: lastSeq, Type: EventTrade, Time: now, OrderID: orderID, Trade: tr})
		resTrades = append(resTrades, tr)
	}
	e.seekSeqLocked(lastSeq)
	if err := e.persistAndApplyLocked(events); err != nil {
		return nil, err
	}
	result.Order = v.orders[orderID]
	result.Trades = resTrades
	result.Seq = prioritySeq
	result.KeptPriority = false
	return result, nil
}

// checkInstructionTargetLocked validates cancel/replace target identity.
func (e *Engine) checkInstructionTargetLocked(orderID string) *RejectError {
	v := e.view
	o, ok := v.orders[orderID]
	if !ok {
		if _, rej := v.rejected[orderID]; rej {
			return rejectf(RejectOrderNotFound, "order %q was rejected and never rested", orderID)
		}
		return rejectf(RejectOrderNotFound, "order %q does not exist", orderID)
	}
	if !o.Active() {
		return rejectf(RejectOrderClosed, "order %q is not active (status from fills/cancel)", orderID)
	}
	return nil
}
