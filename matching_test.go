package matching

import (
	"fmt"
	"testing"
)

// logOrderFlow 打印关键订单流水，便于失败时定位判定依据。
func logOrderFlow(t *testing.T, tag string, r SubmitResult) {
	t.Helper()
	if r.Reason != RejNone {
		t.Logf("[%s] order=%s seq=%d REJECT reason=%s detail=%s",
			tag, r.OrderID, r.Seq, r.Reason, r.Detail)
		return
	}
	t.Logf("[%s] order=%s seq=%d status=%s fills=%d", tag, r.OrderID, r.Seq, r.Status, len(r.Fills))
	for _, f := range r.Fills {
		t.Logf("    FILL taker=%s maker=%s px=%d qty=%d", f.TakerID, f.MakerID, f.Price, f.Qty)
	}
}

func setupTwoParty(t *testing.T, cashA, cashB int64, posB map[string]int64) *Engine {
	t.Helper()
	e, err := NewEngine(Config{STP: STPRejectTaker})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.OpenAccount("A", cashA); err != nil {
		t.Fatal(err)
	}
	if err := e.OpenAccount("B", cashB); err != nil {
		t.Fatal(err)
	}
	for sym, q := range posB {
		if err := e.Deposit("B", sym, q); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// TestTimePriority 同价位严格按提交时间（Seq）先后成交。
func TestTimePriority(t *testing.T) {
	e := setupTwoParty(t, 1_000_000, 1_000_000, map[string]int64{"AAA": 100})
	defer e.Close()

	// B 在 100 元依次挂三笔卖单。
	for i, q := range []int64{2, 3, 5} {
		id := fmt.Sprintf("sell-%d", i)
		r := e.Submit(SubmitOrder{ID: id, Account: "B", Symbol: "AAA",
			Side: Sell, Type: Limit, Price: 100, Qty: q})
		logOrderFlow(t, "rest", r)
		if !r.Accepted {
			t.Fatalf("%s should rest", id)
		}
	}
	// A 以 100 买 6：吃 2+3 共 5，再吃 sell-2 的 1，全部成交。
	r := e.Submit(SubmitOrder{ID: "buy-x", Account: "A", Symbol: "AAA",
		Side: Buy, Type: Limit, Price: 100, Qty: 6})
	logOrderFlow(t, "taker", r)
	if !r.Accepted || r.Status != StatusFilled || len(r.Fills) != 3 {
		t.Fatalf("expect 3 fills and fully filled, got %+v", r)
	}
	if r.Fills[0].MakerID != "sell-0" || r.Fills[0].Qty != 2 {
		t.Fatalf("first fill must hit earliest order sell-0 qty2: %+v", r.Fills[0])
	}
	if r.Fills[1].MakerID != "sell-1" || r.Fills[1].Qty != 3 {
		t.Fatalf("second fill must hit sell-1 qty3: %+v", r.Fills[1])
	}
	if r.Fills[2].MakerID != "sell-2" || r.Fills[2].Qty != 1 {
		t.Fatalf("third fill must hit sell-2 qty1: %+v", r.Fills[2])
	}
	o0, _ := e.GetOrder("sell-0")
	o1, _ := e.GetOrder("sell-1")
	o2, _ := e.GetOrder("sell-2")
	if o0.Status != StatusFilled || o1.Status != StatusFilled {
		t.Fatalf("sell-0/1 should be filled: %s %s", o0.Status, o1.Status)
	}
	if o2.FilledQty != 1 || o2.Remaining() != 4 {
		t.Fatalf("sell-2 partial 1, got filled=%d", o2.FilledQty)
	}
}

// TestPricePriority 价格优先：买单优先吃最低卖价。
func TestPricePriority(t *testing.T) {
	e := setupTwoParty(t, 1_000_000, 1_000_000, map[string]int64{"AAA": 100})
	defer e.Close()

	e.Submit(SubmitOrder{ID: "s101", Account: "B", Symbol: "AAA", Side: Sell, Type: Limit, Price: 101, Qty: 5})
	e.Submit(SubmitOrder{ID: "s100", Account: "B", Symbol: "AAA", Side: Sell, Type: Limit, Price: 100, Qty: 5})
	r := e.Submit(SubmitOrder{ID: "b", Account: "A", Symbol: "AAA", Side: Buy, Type: Limit, Price: 101, Qty: 5})
	logOrderFlow(t, "taker", r)
	if len(r.Fills) != 1 || r.Fills[0].MakerID != "s100" || r.Fills[0].Price != 100 {
		t.Fatalf("must hit best ask s100@100: %+v", r.Fills)
	}
}
