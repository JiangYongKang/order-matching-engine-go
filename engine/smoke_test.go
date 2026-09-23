package engine

import (
	"testing"
)

func TestSmokeCrossing(t *testing.T) {
	e, err := NewEngine(EngineConfig{
		InitialCash:     1_000_000,
		InitialAccounts: []string{"A", "B"},
		InitialPositions: map[string]map[string]int64{
			"B": {"AAA": 1000},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	r1, err := e.Submit(OrderRequest{ID: "o1", Account: "B", Symbol: "AAA", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 10})
	if err != nil || r1.Reject != nil {
		t.Fatalf("sell resting: %v %v", err, r1.Reject)
	}
	if !r1.Resting {
		t.Fatalf("expected resting")
	}

	r2, err := e.Submit(OrderRequest{ID: "o2", Account: "A", Symbol: "AAA", Side: SideBuy, Type: OrderTypeLimit, Price: 101, Qty: 7})
	if err != nil || r2.Reject != nil {
		t.Fatalf("buy crossing: %v %v", err, r2.Reject)
	}
	if len(r2.Trades) != 1 || r2.Trades[0].Qty != 7 || r2.Trades[0].Price != 100 {
		t.Fatalf("unexpected trades: %+v", r2.Trades)
	}
	if r2.Status != StatusFilled {
		t.Fatalf("buy should fill, got %s", r2.Status)
	}
	aA, _ := e.AccountSnapshot("A")
	if aA.Cash != 1_000_000-700 || aA.Positions["AAA"] != 7 {
		t.Fatalf("buyer state wrong: cash=%d pos=%d", aA.Cash, aA.Positions["AAA"])
	}
	aB, _ := e.AccountSnapshot("B")
	if aB.Cash != 1_000_000+700 || aB.Positions["AAA"] != 1000-7 || aB.ReservedPositions["AAA"] != 3 {
		t.Fatalf("seller state wrong: cash=%d pos=%d reserved=%d", aB.Cash, aB.Positions["AAA"], aB.ReservedPositions["AAA"])
	}
}
