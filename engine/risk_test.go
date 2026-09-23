package engine

import "testing"

func riskEngine(t *testing.T, rc *RiskConfig) *Engine {
	t.Helper()
	positions := map[string]map[string]int64{
		"alice": {"X": 100},
		"bob":   {"X": 100},
	}
	e, err := NewEngine(EngineConfig{
		InitialCash:      1_000_000,
		InitialPositions: positions,
		Risk:             rc,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func TestSingleOrderNotionalLimit(t *testing.T) {
	e := riskEngine(t, &RiskConfig{Accounts: map[string]*Limits{
		"alice": {MaxSingleOrderNotional: 500},
	}})
	// price 100 x qty 6 = 600 > 500, even though cash suffices.
	r, err := e.Submit(OrderRequest{ID: "r1", Account: "alice", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 6})
	if err != nil || r.Reject == nil || r.Reject.Reason != RejectRiskSingleOrder {
		t.Fatalf("expected RISK_SINGLE_ORDER_LIMIT, got %+v err=%v", r, err)
	}
	t.Logf("LIMIT single-order reject: %s", r.Reject.Message)
	// price 100 x qty 5 = 500 == limit is allowed.
	if r2, err := e.Submit(OrderRequest{ID: "r2", Account: "alice", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 5}); err != nil || r2.Reject != nil {
		t.Fatalf("boundary qty must pass, got %+v err=%v", r2, err)
	}
	snap, _ := e.AccountSnapshot("alice")
	if snap.ReservedCash != 500 {
		t.Fatalf("accepted boundary order reserves 500, got %d", snap.ReservedCash)
	}
}

func TestGrossExposureLimit(t *testing.T) {
	e := riskEngine(t, &RiskConfig{Accounts: map[string]*Limits{
		"alice": {MaxGrossExposure: 700},
	}})
	mustSubmit(t, e, OrderRequest{ID: "g1", Account: "alice", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 5}) // 500
	// +300 = 800 > 700 reject.
	r, err := e.Submit(OrderRequest{ID: "g2", Account: "alice", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 3})
	if err != nil || r.Reject == nil || r.Reject.Reason != RejectRiskGrossExposure {
		t.Fatalf("expected gross limit reject, got %+v err=%v", r, err)
	}
	t.Logf("LIMIT gross reject: %s", r.Reject.Message)
	// 500 + 200 = 700 boundary passes.
	if r2, err := e.Submit(OrderRequest{ID: "g3", Account: "alice", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 2}); err != nil || r2.Reject != nil {
		t.Fatalf("gross boundary must pass, got %+v", r2)
	}
	// An order that fully crosses contributes no resting notional: it must
	// not be rejected by the gross-exposure rule (cash permitting).
	e2 := riskEngine(t, &RiskConfig{Accounts: map[string]*Limits{
		"alice": {MaxGrossExposure: 100},
	}})
	mustSubmit(t, e2, OrderRequest{ID: "s1", Account: "bob", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 5})
	r3 := mustSubmit(t, e2, OrderRequest{ID: "g4", Account: "alice", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 2})
	if r3.Status != StatusFilled {
		t.Fatalf("crossing order should fill regardless of gross rest limit, got %s", r3.Status)
	}
}

func TestSymbolPositionLimit(t *testing.T) {
	e := riskEngine(t, &RiskConfig{Accounts: map[string]*Limits{
		"alice": {MaxSymbolPosition: map[string]int64{"X": 120}},
	}})
	// Current position 100; buying 30 more would project +130 > 120.
	r, err := e.Submit(OrderRequest{ID: "p1", Account: "alice", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 30})
	if err != nil || r.Reject == nil || r.Reject.Reason != RejectRiskSymbolPosition {
		t.Fatalf("expected symbol position reject, got %+v err=%v", r, err)
	}
	t.Logf("LIMIT symbol-position reject: %s", r.Reject.Message)
	// Selling 200 projects -100, |-100| <= 120 is allowed.
	if r2, err := e.Submit(OrderRequest{ID: "p2", Account: "alice", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 100}); err != nil || r2.Reject != nil {
		t.Fatalf("short side within limit must pass, got %+v", r2)
	}
}

func TestGlobalExposureLimit(t *testing.T) {
	e := riskEngine(t, &RiskConfig{GlobalMaxOpenNotional: 600})
	mustSubmit(t, e, OrderRequest{ID: "w1", Account: "alice", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 3}) // 300
	mustSubmit(t, e, OrderRequest{ID: "w2", Account: "bob", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 3})   // 300
	r, err := e.Submit(OrderRequest{ID: "w3", Account: "alice", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 1})
	if err != nil || r.Reject == nil || r.Reject.Reason != RejectRiskGlobalExposure {
		t.Fatalf("expected global exposure reject, got %+v err=%v", r, err)
	}
	t.Logf("LIMIT global reject: %s", r.Reject.Message)
}

func TestSelfTradeRejectedWithoutFilling(t *testing.T) {
	e := riskEngine(t, nil)
	mustSubmit(t, e, OrderRequest{ID: "st1", Account: "alice", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 99, Qty: 10})
	r, err := e.Submit(OrderRequest{ID: "st2", Account: "alice", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 99, Qty: 10})
	if err != nil || r.Reject == nil || r.Reject.Reason != RejectSelfTrade {
		t.Fatalf("expected SELF_TRADE reject, got %+v", r)
	}
	t.Logf("SELF-TRADE reject: %s", r.Reject.Message)
	snap, _ := e.AccountSnapshot("alice")
	if snap.Positions["X"] != 100 || snap.Cash != 1_000_000 || snap.ReservedCash != 990 || snap.OpenOrders != 1 {
		t.Fatalf("self-trade reject must leave state untouched: %+v", snap)
	}
}

func TestInsufficientFundsAndShares(t *testing.T) {
	// alice has cash 1_000_000 and 100 X.
	e := riskEngine(t, nil)
	// A buy costing 1_000_000 leaves 0: at exactly cash it is allowed.
	mustSubmit(t, e, OrderRequest{ID: "f1", Account: "alice", Symbol: "Y", Side: SideBuy, Type: OrderTypeLimit, Price: 1000, Qty: 1000})
	// No cash left; another buy fails.
	r, err := e.Submit(OrderRequest{ID: "f2", Account: "alice", Symbol: "Y", Side: SideBuy, Type: OrderTypeLimit, Price: 1, Qty: 1})
	if err != nil || r.Reject == nil || r.Reject.Reason != RejectInsufficientFunds {
		t.Fatalf("expected insufficient funds, got %+v", r)
	}
	// Sell more than held fails.
	r2, err := e.Submit(OrderRequest{ID: "f3", Account: "alice", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 10, Qty: 101})
	if err != nil || r2.Reject == nil || r2.Reject.Reason != RejectInsufficientShares {
		t.Fatalf("expected insufficient shares, got %+v", r2)
	}
}
