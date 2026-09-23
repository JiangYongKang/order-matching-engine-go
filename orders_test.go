package matching

import (
	"reflect"
	"testing"
)

func snap(t *testing.T, e *Engine) EngineSnapshot {
	t.Helper()
	s := e.Snapshot()
	t.Logf("snapshot seq=%d accounts=%v orders=%d books=%v",
		s.Seq, s.Accounts, len(s.Orders), s.Books)
	return s
}

// TestRejectPaths 各类非法/不足指令必须被可区分地拒绝，且状态不变。
func TestRejectPaths(t *testing.T) {
	e := setupTwoParty(t, 1000, 1000, map[string]int64{"AAA": 5})
	defer e.Close()

	before := snap(t, e)

	cases := []struct {
		name string
		o    SubmitOrder
		want RejectReason
	}{
		{"zero qty", SubmitOrder{ID: "x1", Account: "A", Symbol: "AAA", Side: Buy, Type: Limit, Price: 10, Qty: 0}, RejInvalidQuantity},
		{"negative qty", SubmitOrder{ID: "x2", Account: "A", Symbol: "AAA", Side: Buy, Type: Limit, Price: 10, Qty: -3}, RejInvalidQuantity},
		{"bad limit price", SubmitOrder{ID: "x3", Account: "A", Symbol: "AAA", Side: Buy, Type: Limit, Price: 0, Qty: 1}, RejInvalidPrice},
		{"unknown account", SubmitOrder{ID: "x4", Account: "Z", Symbol: "AAA", Side: Buy, Type: Limit, Price: 10, Qty: 1}, RejUnknownAccount},
		{"insufficient cash", SubmitOrder{ID: "x5", Account: "A", Symbol: "AAA", Side: Buy, Type: Limit, Price: 100, Qty: 100}, RejInsufficientFunds},
		{"insufficient position", SubmitOrder{ID: "x6", Account: "B", Symbol: "AAA", Side: Sell, Type: Limit, Price: 10, Qty: 50}, RejInsufficientPosition},
		{"market no liquidity buy", SubmitOrder{ID: "x7", Account: "A", Symbol: "NOLIQ", Side: Buy, Type: Market, Qty: 1}, RejNoLiquidity},
		{"market no liquidity sell", SubmitOrder{ID: "x8", Account: "A", Symbol: "NOLIQ", Side: Sell, Type: Market, Qty: 1}, RejInsufficientPosition},
	}
	for _, c := range cases {
		r := e.Submit(c.o)
		logOrderFlow(t, c.name, r)
		if r.Accepted || r.Reason != c.want {
			t.Fatalf("%s: want reason=%s got accepted=%v reason=%s", c.name, c.want, r.Accepted, r.Reason)
		}
	}

	// 全部拒绝后，账户与订单簿必须与之前完全一致。
	after := snap(t, e)
	if after.Seq != before.Seq+int64(len(cases)) {
		t.Fatalf("seq advances only for journaled rejects: before=%d after=%d", before.Seq, after.Seq)
	}
	ab := before.Accounts["A"]
	aa := after.Accounts["A"]
	if !reflect.DeepEqual(ab, aa) {
		t.Fatalf("account A changed by rejects:\nbefore=%+v\nafter =%+v", ab, aa)
	}
	bb := before.Accounts["B"]
	ba := after.Accounts["B"]
	if !reflect.DeepEqual(bb, ba) {
		t.Fatalf("account B changed by rejects:\nbefore=%+v\nafter =%+v", bb, ba)
	}
	activeBefore := activeOrderCount(before.Orders)
	activeAfter := activeOrderCount(after.Orders)
	if activeBefore != activeAfter {
		t.Fatalf("active order count changed: before=%d after=%d (rejects are retained as REJECTED)",
			activeBefore, activeAfter)
	}
	for _, id := range []string{"x1", "x2", "x3", "x4", "x5", "x6", "x7", "x8"} {
		o, ok := after.Orders[id]
		if !ok || o.Status != StatusRejected {
			t.Fatalf("rejected order %s must be recorded REJECTED", id)
		}
	}
	// 重复订单号必须可区分地拒绝。
	ok1 := e.Submit(SubmitOrder{ID: "dup", Account: "A", Symbol: "AAA", Side: Buy, Type: Limit, Price: 1, Qty: 1})
	if !ok1.Accepted {
		t.Fatal("first dup submit should pass")
	}
	ok2 := e.Submit(SubmitOrder{ID: "dup", Account: "A", Symbol: "AAA", Side: Buy, Type: Limit, Price: 1, Qty: 1})
	if ok2.Reason != RejDuplicateOrderID {
		t.Fatalf("duplicate id reason = %s", ok2.Reason)
	}
	t.Logf("duplicate rejected: %s", ok2.Detail)
}

func activeOrderCount(orders map[string]Order) int {
	n := 0
	for _, o := range orders {
		if o.Status != StatusRejected {
			n++
		}
	}
	return n
}

// TestMarketOrders 市价单：即时成交、部分成交后 IOC 撤销剩余、资金约束截断。
func TestMarketOrders(t *testing.T) {
	e := setupTwoParty(t, 1000, 0, map[string]int64{"AAA": 100})
	defer e.Close()

	// 卖盘：100@10 股、110@10 股（B 只有 AAA 持仓，再让 C 挂第二档）
	e.OpenAccount("C", 0)
	e.Deposit("C", "AAA", 10)
	e.Submit(SubmitOrder{ID: "s100", Account: "B", Symbol: "AAA", Side: Sell, Type: Limit, Price: 100, Qty: 4})
	e.Submit(SubmitOrder{ID: "s110", Account: "C", Symbol: "AAA", Side: Sell, Type: Limit, Price: 110, Qty: 10})

	// A 现金 1000：市价买 20，100 元吃 4 股花 400，110 元只能再吃 5 股（550），剩余 IOC。
	r := e.Submit(SubmitOrder{ID: "m1", Account: "A", Symbol: "AAA", Side: Buy, Type: Market, Qty: 20})
	logOrderFlow(t, "market buy", r)
	if !r.Accepted || r.Status != StatusCancelled || r.FilledQtyInResult() != 9 {
		t.Fatalf("market buy should fill 9 then IOC cancel: %+v", r)
	}
	accA, _ := e.GetAccount("A")
	if accA.Cash != 50 || accA.Positions["AAA"] != 9 {
		t.Fatalf("A cash=%d pos=%d want 50/9", accA.Cash, accA.Positions["AAA"])
	}
	s110, _ := e.GetOrder("s110")
	if s110.FilledQty != 5 || s110.Remaining() != 5 {
		t.Fatalf("s110 partial 5: %+v", s110)
	}

	// 市价卖：D 在 90 挂足买单，A 只有 9 股可用持仓，卖 20 只能成交 9，剩余 IOC。
	e.OpenAccount("D", 100000)
	e.Submit(SubmitOrder{ID: "bid90d", Account: "D", Symbol: "AAA", Side: Buy, Type: Limit, Price: 90, Qty: 100})
	r3 := e.Submit(SubmitOrder{ID: "m2", Account: "A", Symbol: "AAA", Side: Sell, Type: Market, Qty: 20})
	logOrderFlow(t, "market sell", r3)
	if !r3.Accepted || r3.Status != StatusCancelled || r3.FilledQtyInResult() != 9 {
		t.Fatalf("market sell fills 9 (only free position), IOC rest: %+v", r3)
	}
	accA2, _ := e.GetAccount("A")
	if accA2.Positions["AAA"] != 0 {
		t.Fatalf("A should have no position left, got %d", accA2.Positions["AAA"])
	}
}

// FilledQtyInResult 由成交列表汇总成交数量。
func (r SubmitResult) FilledQtyInResult() int64 {
	var q int64
	for _, f := range r.Fills {
		q += f.Qty
	}
	return q
}

// TestCancel 撤单释放冻结，已终结订单不可撤。
func TestCancel(t *testing.T) {
	e := setupTwoParty(t, 1000, 0, map[string]int64{"AAA": 10})
	defer e.Close()

	r := e.Submit(SubmitOrder{ID: "buy1", Account: "A", Symbol: "AAA", Side: Buy, Type: Limit, Price: 50, Qty: 10})
	if !r.Accepted {
		t.Fatal(r)
	}
	a1, _ := e.GetAccount("A")
	if a1.Cash != 500 || a1.ReservedCash != 500 {
		t.Fatalf("after rest cash=%d reserved=%d", a1.Cash, a1.ReservedCash)
	}
	cr := e.Cancel(CancelOrder{ID: "buy1"})
	logOrderFlow(t, "cancel", cr)
	if !cr.Accepted {
		t.Fatalf("cancel: %+v", cr)
	}
	a2, _ := e.GetAccount("A")
	if a2.Cash != 1000 || a2.ReservedCash != 0 {
		t.Fatalf("after cancel cash=%d reserved=%d", a2.Cash, a2.ReservedCash)
	}
	book := e.BookSnapshot("AAA", 5)
	if len(book.Bids) != 0 {
		t.Fatalf("book should be empty after cancel: %+v", book)
	}
	if r2 := e.Cancel(CancelOrder{ID: "buy1"}); r2.Reason != RejOrderClosed {
		t.Fatalf("double cancel reason=%s", r2.Reason)
	}
	if r3 := e.Cancel(CancelOrder{ID: "nope"}); r3.Reason != RejUnknownOrder {
		t.Fatalf("cancel unknown reason=%s", r3.Reason)
	}
}

// TestAmendPriority 同价改量保留优先级；改价被拒；增改量需资金校验。
func TestAmendPriority(t *testing.T) {
	e := setupTwoParty(t, 1000, 100000, map[string]int64{})
	defer e.Close()
	e.OpenAccount("C", 0)
	if err := e.Deposit("C", "AAA", 100); err != nil {
		t.Fatal(err)
	}

	// A 在 100 挂买 5（seq 较早），B 也在 100 挂买 5（seq 较晚）。
	e.Submit(SubmitOrder{ID: "a1", Account: "A", Symbol: "AAA", Side: Buy, Type: Limit, Price: 100, Qty: 5})
	e.Submit(SubmitOrder{ID: "b1", Account: "B", Symbol: "AAA", Side: Buy, Type: Limit, Price: 100, Qty: 5})

	// A 改量到 8：仍排在 B 前面。随后 C 以 100 卖 10，
	// 必须先完全满足 A 的 8，再成交 B 的 2。
	r := e.Amend(AmendOrder{ID: "a1", NewQty: 8})
	if !r.Accepted {
		t.Fatalf("amend up: %+v", r)
	}
	a, _ := e.GetAccount("A")
	if a.ReservedCash != 800 || a.Cash != 200 {
		t.Fatalf("A after amend up cash=%d reserved=%d", a.Cash, a.ReservedCash)
	}
	sr := e.Submit(SubmitOrder{ID: "sellc", Account: "C", Symbol: "AAA", Side: Sell, Type: Limit, Price: 100, Qty: 10})
	logOrderFlow(t, "sell after amend", sr)
	if sr.FilledQtyInResult() != 10 || len(sr.Fills) != 2 {
		t.Fatalf("expect 2 fills total 10: %+v", sr.Fills)
	}
	if sr.Fills[0].MakerID != "a1" || sr.Fills[0].Qty != 8 {
		t.Fatalf("amended order keeps priority, want a1@8 first: %+v", sr.Fills[0])
	}
	if sr.Fills[1].MakerID != "b1" || sr.Fills[1].Qty != 2 {
		t.Fatalf("then b1@2: %+v", sr.Fills[1])
	}

	// 改价必须被拒。
	if pr := e.Amend(AmendOrder{ID: "b1", NewPrice: 99, NewQty: 3}); pr.Reason != RejAmendPriceChange {
		t.Fatalf("price change reason=%s", pr.Reason)
	}
	// 增改量资金不足必须拒绝且不改状态。
	before, _ := e.GetAccount("B")
	if ir := e.Amend(AmendOrder{ID: "b1", NewQty: 2000}); ir.Reason != RejInsufficientFunds {
		t.Fatalf("amend beyond cash reason=%s detail=%s", ir.Reason, ir.Detail)
	}
	ob, _ := e.GetOrder("b1")
	if ob.Remaining() != 3 {
		// b1 已成交 2，剩 3；拒绝增改量后必须仍是 3。
		t.Fatalf("b1 remaining=%d want 3 after rejected amend", ob.Remaining())
	}
	after, _ := e.GetAccount("B")
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("account changed after rejected amend:\n%+v\n%+v", before, after)
	}
	// 缩量释放冻结。
	r2 := e.Amend(AmendOrder{ID: "b1", NewQty: 2}) // FilledQty=2，不能 <= filled
	if r2.Reason != RejAmendInvalidQty {
		t.Fatalf("amend to filled qty must be invalid: %s", r2.Reason)
	}
	r3 := e.Amend(AmendOrder{ID: "b1", NewQty: 4})
	if !r3.Accepted {
		t.Fatalf("amend shrink: %+v", r3)
	}
	b2, _ := e.GetAccount("B")
	if b2.ReservedCash != 200 {
		t.Fatalf("B reserved after shrink to remaining 2 = %d want 200", b2.ReservedCash)
	}
	t.Logf("amend priority verified; B final cash=%d reserved=%d", b2.Cash, b2.ReservedCash)
}
