package matching

import (
	"testing"
)

func newSmokeEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := NewEngine(Config{STP: STPRejectTaker})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.OpenAccount("A", 100_000); err != nil {
		t.Fatal(err)
	}
	if err := e.OpenAccount("B", 100_000); err != nil {
		t.Fatal(err)
	}
	if err := e.Deposit("B", "AAA", 10); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestSmokeLimitCross(t *testing.T) {
	e := newSmokeEngine(t)
	defer e.Close()

	r1 := e.Submit(SubmitOrder{ID: "sell-1", Account: "B", Symbol: "AAA",
		Side: Sell, Type: Limit, Price: 100, Qty: 10})
	if !r1.Accepted || r1.Status != StatusNew {
		t.Fatalf("sell resting: %+v", r1)
	}
	r2 := e.Submit(SubmitOrder{ID: "buy-1", Account: "A", Symbol: "AAA",
		Side: Buy, Type: Limit, Price: 100, Qty: 7})
	if !r2.Accepted || len(r2.Fills) != 1 || r2.Fills[0].Qty != 7 {
		t.Fatalf("buy cross: %+v", r2)
	}
	sell, _ := e.GetOrder("sell-1")
	if sell.FilledQty != 7 || sell.Remaining() != 3 || sell.Status != StatusPartiallyFilled {
		t.Fatalf("sell remainder: %+v", sell)
	}
	accA, _ := e.GetAccount("A")
	accB, _ := e.GetAccount("B")
	if accA.Positions["AAA"] != 7 {
		t.Fatalf("A position = %d", accA.Positions["AAA"])
	}
	// B: 冻结 10 股，成交 7 股释放并交割 => 可用剩 0、冻结剩 3
	if accB.Positions["AAA"] != 0 || accB.ReservedPos["AAA"] != 3 {
		t.Fatalf("B positions: free=%d reserved=%d", accB.Positions["AAA"], accB.ReservedPos["AAA"])
	}
	if accB.Cash != 100_700 {
		t.Fatalf("B cash = %d, want 100700", accB.Cash)
	}
	// A: 冻结买单 700，成交后现金 100000-700 = 99300
	if accA.Cash != 99_300 {
		t.Fatalf("A cash = %d, want 99300", accA.Cash)
	}
	t.Logf("fills=%+v book=%+v", r2.Fills, e.BookSnapshot("AAA", 5))
}
