package matching

import (
	"sync"
	"testing"
)

// TestCancelFillRace 并发场景下：同一挂单同时被吃单与撤单，
// 无论谁先进入临界区，都不允许重复成交或冻结泄漏。
// 两种可能结论都合法（撤单成功 or 已成交/终结），但资金必须守恒。
func TestCancelFillRace(t *testing.T) {
	for iter := 0; iter < 20; iter++ {
		e := setupTwoParty(t, 1_000_000, 1_000_000, map[string]int64{"AAA": 100})
		var wg sync.WaitGroup
		var cr, fr SubmitResult

		// 每轮重置：A 与 B 各放一笔，C 抢着吃 / A 抢着撤。
		e.OpenAccount("C", 1_000_000)
		rest := e.Submit(SubmitOrder{ID: "r", Account: "B", Symbol: "AAA",
			Side: Sell, Type: Limit, Price: 100, Qty: 10})
		if !rest.Accepted {
			t.Fatal(rest)
		}

		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			cr = e.Cancel(CancelOrder{ID: "r"})
		}()
		go func() {
			defer wg.Done()
			<-start
			fr = e.Submit(SubmitOrder{ID: "take", Account: "C", Symbol: "AAA",
				Side: Buy, Type: Limit, Price: 100, Qty: 10})
		}()
		close(start)
		wg.Wait()

		o, _ := e.GetOrder("r")
		t.Logf("iter=%d cancel.accepted=%v reason=%s | fill.accepted=%v qty=%d | order status=%s filled=%d",
			iter, cr.Accepted, cr.Reason, fr.Accepted, fr.FilledQtyInResult(), o.Status, o.FilledQty)

		// 不变量：订单成交量 + 撤销剩余 = 原始 10，且只能有一种归宿。
		var cancelledQty int64
		if cr.Accepted {
			cancelledQty = 10 - o.FilledQty
		}
		if o.FilledQty+cancelledQty != 10 {
			t.Fatalf("iter %d: filled=%d cancelled=%d do not sum to 10",
				iter, o.FilledQty, cancelledQty)
		}
		// 成交必须恰好等于吃单结果中的成交总量。
		if fr.Accepted && fr.FilledQtyInResult() != o.FilledQty {
			t.Fatalf("iter %d: taker fills %d but maker recorded %d",
				iter, fr.FilledQtyInResult(), o.FilledQty)
		}
		// 撤单与成交互斥：撤单成功则吃单必须未成交该订单（或吃单本身无对手）。
		if cr.Accepted && o.FilledQty != 0 {
			t.Fatalf("iter %d: cancel succeeded but order also filled %d", iter, o.FilledQty)
		}
		// 持仓守恒（含已成交转移给买方的部分）：
		// B 的 可用+冻结+成交量 == 初始 100（C 原有 0 股 AAA）。
		b, _ := e.GetAccount("B")
		if b.Positions["AAA"]+b.ReservedPos["AAA"]+o.FilledQty != 100 {
			t.Fatalf("iter %d: B position not conserved: free=%d reserved=%d sold=%d",
				iter, b.Positions["AAA"], b.ReservedPos["AAA"], o.FilledQty)
		}
		// C 的资金守恒：现金 + 买单冻结 + 买入花费 == 初始 1_000_000。
		c, _ := e.GetAccount("C")
		spent := int64(0)
		for _, f := range fr.Fills {
			spent += f.Price * f.Qty
		}
		if c.Cash+c.ReservedCash+spent != 1_000_000 {
			t.Fatalf("iter %d: C cash not conserved: cash=%d reserved=%d spent=%d",
				iter, c.Cash, c.ReservedCash, spent)
		}
		e.Close()
	}
}

// TestAmendFillRace 改单（增量）与吃单并发：要么增量生效、要么订单已部分成交，
// 不允许冻结额与订单剩余名义额不一致。
func TestAmendFillRace(t *testing.T) {
	for iter := 0; iter < 20; iter++ {
		e := setupTwoParty(t, 1_000_000, 1_000_000, map[string]int64{"AAA": 100})
		e.OpenAccount("C", 1_000_000)
		e.Submit(SubmitOrder{ID: "a", Account: "A", Symbol: "AAA",
			Side: Buy, Type: Limit, Price: 100, Qty: 5})

		var ar, fr SubmitResult
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			ar = e.Amend(AmendOrder{ID: "a", NewQty: 8})
		}()
		go func() {
			defer wg.Done()
			<-start
			fr = e.Submit(SubmitOrder{ID: "s", Account: "B", Symbol: "AAA",
				Side: Sell, Type: Limit, Price: 100, Qty: 8})
		}()
		close(start)
		wg.Wait()

		o, _ := e.GetOrder("a")
		a, _ := e.GetAccount("A")
		t.Logf("iter=%d amend=%v reason=%s sellFilled=%d order filled=%d remain=%d reservedCash=%d cash=%d",
			iter, ar.Accepted, ar.Reason, fr.FilledQtyInResult(), o.FilledQty, o.Remaining(),
			a.ReservedCash, a.Cash)

		// 不变量：买单冻结现金必须恰好等于剩余量 * 挂单价。
		wantReserved := o.Remaining() * 100
		if a.ReservedCash != wantReserved {
			t.Fatalf("iter %d: reserved cash %d != remaining notional %d (order=%+v)",
				iter, a.ReservedCash, wantReserved, o)
		}
		// 资金守恒：现金 + 买单冻结 + 买入花费 == 初始 1_000_000。
		spentA := int64(0)
		for _, f := range fr.Fills {
			if f.MakerID == "a" { // a 是被动方
				spentA += f.Price * f.Qty
			}
		}
		if a.Cash+a.ReservedCash+spentA != 1_000_000 {
			t.Fatalf("iter %d: A cash+reserved+spent = %d, want 1000000 (cash=%d resv=%d spent=%d)",
				iter, a.Cash+a.ReservedCash+spentA, a.Cash, a.ReservedCash, spentA)
		}
		// 卖方成交总量必须等于买方成交量。
		if got := fr.FilledQtyInResult(); got != o.FilledQty {
			t.Fatalf("iter %d: seller fills %d != buyer filled %d", iter, got, o.FilledQty)
		}
		e.Close()
	}
}
