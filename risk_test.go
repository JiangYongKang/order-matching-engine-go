package matching

import (
	"reflect"
	"testing"
)

// TestRiskLimits 三类限额必须在提交前判定、拒绝原因可区分且不留中间状态。
func TestRiskLimits(t *testing.T) {
	e, err := NewEngine(Config{
		STP: STPRejectTaker,
		Limits: RiskLimits{
			MaxAccountBuyNotional: 1000, // A 买单冻结名义额上限 1000
			MaxSymbolPosition:     8,    // 单账户单标的净持仓上限 8
			MaxGlobalExposure:     5000,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if err := e.OpenAccount("A", 100_000); err != nil {
		t.Fatal(err)
	}
	if err := e.OpenAccount("B", 100_000); err != nil {
		t.Fatal(err)
	}
	if err := e.Deposit("B", "AAA", 100); err != nil {
		t.Fatal(err)
	}

	// 账户级：A 买单 @100 总量不得超过 1000 名义额；同时受标的上限 8 约束，
	// 故先用 8 股（800）建仓，再挂 3 股：800+300>1000 触发账户级限额。
	r1 := e.Submit(SubmitOrder{ID: "a-buy-8", Account: "A", Symbol: "AAA",
		Side: Buy, Type: Limit, Price: 90, Qty: 8})
	logOrderFlow(t, "account-limit ok", r1)
	if !r1.Accepted {
		t.Fatalf("8*90=720 within limits: %+v", r1)
	}
	r2 := e.Submit(SubmitOrder{ID: "a-buy-3", Account: "A", Symbol: "BBB",
		Side: Buy, Type: Limit, Price: 100, Qty: 3}) // 换标的以避开单标的持仓上限
	logOrderFlow(t, "account-limit over", r2)
	if r2.Reason != RejRiskAccountLimit {
		t.Fatalf("720+300 > 1000 account notional: reason=%s", r2.Reason)
	}

	// 标的级：新开账户 C，买入数量按「净持仓 + 数量」判定，上限 8。
	e.OpenAccount("C", 100_000)
	r3 := e.Submit(SubmitOrder{ID: "c-buy-8", Account: "C", Symbol: "AAA",
		Side: Buy, Type: Limit, Price: 10, Qty: 8}) // 名义额 80，低于账户上限；挂簿
	if !r3.Accepted {
		t.Fatalf("qty 8 at symbol limit should be accepted: %+v", r3)
	}
	r4 := e.Submit(SubmitOrder{ID: "c-buy-9", Account: "C", Symbol: "AAA",
		Side: Buy, Type: Limit, Price: 10, Qty: 9})
	logOrderFlow(t, "symbol-limit over", r4)
	if r4.Reason != RejRiskSymbolLimit {
		t.Fatalf("qty 9 exceeds symbol position cap: reason=%s", r4.Reason)
	}

	// 限额触发后簿结构不变：AAA bid 为 90 档(A 8) 与 10 档(C 8)。
	book := e.BookSnapshot("AAA", 10)
	if len(book.Bids) != 2 {
		t.Fatalf("bids after rejected risk orders = %+v", book.Bids)
	}
	t.Logf("account/symbol limits verified; book bids=%+v exposure=%d",
		book.Bids, e.globalExposure())

	// 全局敞口在只配置全局限额的独立引擎中验证，避免与账户限额混淆。
	ge, err := NewEngine(Config{STP: STPRejectTaker,
		Limits: RiskLimits{MaxGlobalExposure: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	defer ge.Close()
	ge.OpenAccount("X", 1_000_000)
	ge.OpenAccount("Y", 1_000_000)
	ge.Deposit("Y", "ZZZ", 100)
	gr1 := ge.Submit(SubmitOrder{ID: "x1", Account: "X", Symbol: "ZZZ",
		Side: Buy, Type: Limit, Price: 100, Qty: 8}) // 800
	if !gr1.Accepted {
		t.Fatalf("800 within global 1000: %+v", gr1)
	}
	gr2 := ge.Submit(SubmitOrder{ID: "x2", Account: "X", Symbol: "ZZZ",
		Side: Buy, Type: Limit, Price: 100, Qty: 3}) // 800+300>1000
	logOrderFlow(t, "global-limit over", gr2)
	if gr2.Reason != RejRiskGlobalLimit {
		t.Fatalf("global exposure breach: reason=%s", gr2.Reason)
	}
	if gb := ge.BookSnapshot("ZZZ", 5); len(gb.Bids) != 1 || gb.Bids[0].Qty != 8 {
		t.Fatalf("book unchanged after global-limit reject: %+v", gb.Bids)
	}
}

// TestRiskLimitReleasedAfterFill 成交/撤单释放额度后，新订单可再次提交。
func TestRiskLimitReleasedAfterFill(t *testing.T) {
	e, err := NewEngine(Config{STP: STPRejectTaker,
		Limits: RiskLimits{MaxAccountBuyNotional: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.OpenAccount("A", 100_000)
	e.OpenAccount("B", 100_000)
	e.Deposit("B", "AAA", 100)

	e.Submit(SubmitOrder{ID: "a1", Account: "A", Symbol: "AAA", Side: Buy, Type: Limit, Price: 100, Qty: 10})
	// 再挂 1 股超限被拒。
	if r := e.Submit(SubmitOrder{ID: "a2", Account: "A", Symbol: "AAA", Side: Buy, Type: Limit, Price: 100, Qty: 1}); r.Reason != RejRiskAccountLimit {
		t.Fatalf("expect account limit, got %s", r.Reason)
	}
	// B 卖掉 10 股给 a1，a1 全成交后买单额度归零。
	e.Submit(SubmitOrder{ID: "s1", Account: "B", Symbol: "AAA", Side: Sell, Type: Limit, Price: 100, Qty: 10})
	a1, _ := e.GetOrder("a1")
	if a1.Status != StatusFilled {
		t.Fatalf("a1 should be filled: %s", a1.Status)
	}
	// 现在额度空闲，可再挂 10 股。
	r := e.Submit(SubmitOrder{ID: "a3", Account: "A", Symbol: "AAA", Side: Buy, Type: Limit, Price: 100, Qty: 10})
	if !r.Accepted {
		t.Fatalf("notional freed after fill: %+v", r)
	}
	t.Log("buy notional limit released after fill OK")
}

// TestSelfTradePolicies 两种 STP 策略：拒绝主动单零副作用；撤被动方后继续撮合。
func TestSelfTradePolicies(t *testing.T) {
	// 策略一：STPRejectTaker
	e := setupTwoParty(t, 100_000, 100_000, map[string]int64{"AAA": 50})
	defer e.Close()
	if err := e.Deposit("A", "AAA", 50); err != nil {
		t.Fatal(err)
	}
	r1 := e.Submit(SubmitOrder{ID: "self-sell", Account: "A", Symbol: "AAA",
		Side: Sell, Type: Limit, Price: 100, Qty: 10})
	if !r1.Accepted {
		t.Fatal(r1)
	}
	before := e.Snapshot()
	r2 := e.Submit(SubmitOrder{ID: "self-buy", Account: "A", Symbol: "AAA",
		Side: Buy, Type: Limit, Price: 100, Qty: 10})
	logOrderFlow(t, "stp reject taker", r2)
	if r2.Reason != RejSelfTrade {
		t.Fatalf("self-trade reject: %s", r2.Reason)
	}
	// 被动单必须原样保留，账户状态不变。
	o, _ := e.GetOrder("self-sell")
	if o.Status != StatusNew || o.FilledQty != 0 {
		t.Fatalf("maker untouched: %+v", o)
	}
	after := e.Snapshot()
	if !reflect.DeepEqual(before.Accounts["A"], after.Accounts["A"]) {
		t.Fatalf("STP reject left side effects on account:\n%+v\n%+v",
			before.Accounts["A"], after.Accounts["A"])
	}
	if !reflect.DeepEqual(before.Books["AAA"], after.Books["AAA"]) {
		t.Fatalf("STP reject changed order book:\n%+v\n%+v",
			before.Books["AAA"], after.Books["AAA"])
	}

	// 策略二：STPCancelMaker
	e2, err := NewEngine(Config{STP: STPCancelMaker})
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	e2.OpenAccount("A", 100_000)
	e2.OpenAccount("B", 0)
	e2.Deposit("A", "AAA", 50)
	e2.Deposit("B", "AAA", 50)
	// A 自己在 100 卖 5；B 在 100 卖 5 排在其后（价格相同按时间）。
	e2.Submit(SubmitOrder{ID: "a-sell", Account: "A", Symbol: "AAA", Side: Sell, Type: Limit, Price: 100, Qty: 5})
	e2.Submit(SubmitOrder{ID: "b-sell", Account: "B", Symbol: "AAA", Side: Sell, Type: Limit, Price: 100, Qty: 5})
	r := e2.Submit(SubmitOrder{ID: "a-buy", Account: "A", Symbol: "AAA", Side: Buy, Type: Limit, Price: 100, Qty: 8})
	logOrderFlow(t, "stp cancel maker", r)
	if !r.Accepted {
		t.Fatalf("cancel-maker STP should proceed: %+v", r)
	}
	// a-sell 必须已撤销且冻结释放；买单吃 B 的 5 股，剩余 3 股挂簿。
	as, _ := e2.GetOrder("a-sell")
	if as.Status != StatusCancelled {
		t.Fatalf("a-sell should be STP cancelled: %s", as.Status)
	}
	bs, _ := e2.GetOrder("b-sell")
	if bs.Status != StatusFilled || bs.FilledQty != 5 {
		t.Fatalf("b-sell filled 5 after STP maker cancel: %+v", bs)
	}
	ab, _ := e2.GetOrder("a-buy")
	if ab.FilledQty != 5 || ab.Remaining() != 3 || ab.Status != StatusPartiallyFilled {
		t.Fatalf("a-buy fills 5 rests 3: %+v", ab)
	}
	aa, _ := e2.GetAccount("A")
	// A 初始现金 100000、持仓 50：自卖单撤销释放 5 股冻结；
	// 买 8 股先冻结 800，成交 5 股花 500，剩 3 股冻结 300，故可用现金 99200。
	if aa.Cash != 99_200 {
		t.Fatalf("A cash=%d want 99200", aa.Cash)
	}
	if aa.Positions["AAA"] != 55 { // 50 可用（5 股冻结已释放）+ 5 买入
		t.Fatalf("A free pos=%d want 55", aa.Positions["AAA"])
	}
	if aa.ReservedPos["AAA"] != 0 { // a-sell 已撤，卖单冻结全部释放
		t.Fatalf("A reserved pos=%d want 0 after self-sell cancelled", aa.ReservedPos["AAA"])
	}
	if aa.ReservedCash != 300 { // a-buy 剩余 3 股买单冻结 3*100
		t.Fatalf("A reserved cash for resting buy 3 = %d want 300", aa.ReservedCash)
	}
	t.Logf("STP cancel-maker OK; A cash=%d freePos=%d reservedCash=%d",
		aa.Cash, aa.Positions["AAA"], aa.ReservedCash)
}
