package engine

import (
	"testing"
)

func newTestEngine(t *testing.T, accounts ...string) *Engine {
	t.Helper()
	positions := make(map[string]map[string]int64, len(accounts))
	for _, a := range accounts {
		positions[a] = map[string]int64{"X": 10000, "AAA": 10000, "Y": 10000}
	}
	e, err := NewEngine(EngineConfig{
		InitialCash:      1_000_000,
		InitialPositions: positions,
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func mustSubmit(t *testing.T, e *Engine, req OrderRequest) *SubmitResult {
	t.Helper()
	r, err := e.Submit(req)
	if err != nil {
		t.Fatalf("submit %s: %v", req.ID, err)
	}
	if r.Reject != nil {
		t.Fatalf("submit %s rejected: %v", req.ID, r.Reject)
	}
	t.Logf("SUBMIT %s %s %s price=%d qty=%d -> status=%s resting=%v trades=%d",
		req.ID, req.Side, req.Symbol, req.Price, req.Qty, r.Status, r.Resting, len(r.Trades))
	for _, tr := range r.Trades {
		t.Logf("  TRADE seq=%d %s price=%d qty=%d maker=%s taker=%s",
			tr.Seq, tr.Symbol, tr.Price, tr.Qty, tr.MakerOrderID, tr.TakerOrderID)
	}
	return r
}

func mustReject(t *testing.T, e *Engine, req OrderRequest, want RejectReason) {
	t.Helper()
	r, err := e.Submit(req)
	if err != nil {
		t.Fatalf("submit %s error: %v", req.ID, err)
	}
	if r.Reject == nil {
		t.Fatalf("submit %s expected reject %s, got status %s", req.ID, want, r.Status)
	}
	if r.Reject.Reason != want {
		t.Fatalf("submit %s reason=%s want %s (%s)", req.ID, r.Reject.Reason, want, r.Reject.Message)
	}
	t.Logf("REJECT %s reason=%s detail=%q", req.ID, r.Reject.Reason, r.Reject.Message)
}

func TestTimePrioritySameLevel(t *testing.T) {
	e := newTestEngine(t, "A", "B", "C", "D")
	// Two sellers at the same price; earlier order must fill first.
	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "B", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 5})
	mustSubmit(t, e, OrderRequest{ID: "s2", Account: "C", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 5})

	r := mustSubmit(t, e, OrderRequest{ID: "b1", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 7})
	if len(r.Trades) != 2 {
		t.Fatalf("want 2 trades, got %d", len(r.Trades))
	}
	if r.Trades[0].MakerOrderID != "s1" || r.Trades[0].Qty != 5 {
		t.Fatalf("first fill must be s1 x5, got %s x%d", r.Trades[0].MakerOrderID, r.Trades[0].Qty)
	}
	if r.Trades[1].MakerOrderID != "s2" || r.Trades[1].Qty != 2 {
		t.Fatalf("second fill must be s2 x2, got %s x%d", r.Trades[1].MakerOrderID, r.Trades[1].Qty)
	}
	// s2 keeps remaining 3 with its original priority; a later buy hits it first.
	mustSubmit(t, e, OrderRequest{ID: "s3", Account: "D", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 3})
	r2 := mustSubmit(t, e, OrderRequest{ID: "b2", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 3})
	if r2.Trades[0].MakerOrderID != "s2" {
		t.Fatalf("s2 partial must retain priority over s3, got %s", r2.Trades[0].MakerOrderID)
	}
}

func TestPricePriority(t *testing.T) {
	e := newTestEngine(t, "A", "B", "C")
	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "B", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 101, Qty: 5})
	mustSubmit(t, e, OrderRequest{ID: "s2", Account: "C", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 99, Qty: 5})
	r := mustSubmit(t, e, OrderRequest{ID: "b1", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 5})
	if len(r.Trades) != 1 || r.Trades[0].MakerOrderID != "s2" || r.Trades[0].Price != 99 {
		t.Fatalf("must hit cheapest ask s2@99, got %+v", r.Trades)
	}
}

func TestNoCrossRestsAndCancel(t *testing.T) {
	e := newTestEngine(t, "A", "B")
	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "B", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 110, Qty: 5})
	r := mustSubmit(t, e, OrderRequest{ID: "b1", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 5})
	if !r.Resting || len(r.Trades) != 0 {
		t.Fatalf("non-crossing buy must rest")
	}
	// Reservation is active before cancel.
	snap, _ := e.AccountSnapshot("A")
	if snap.ReservedCash != 500 || snap.Cash != 1_000_000 {
		t.Fatalf("expected 500 reserved, got %+v", snap)
	}
	cr, err := e.Cancel("b1")
	if err != nil || cr.Reject != nil {
		t.Fatalf("cancel: %v %v", err, cr)
	}
	snap, _ = e.AccountSnapshot("A")
	if snap.ReservedCash != 0 {
		t.Fatalf("cancel must release reservation, got %+v", snap)
	}
	if cr2, _ := e.Cancel("b1"); cr2.Reject == nil || cr2.Reject.Reason != RejectOrderClosed {
		t.Fatalf("double cancel must reject closed, got %+v", cr2)
	}
	if cr3, _ := e.Cancel("nope"); cr3.Reject == nil || cr3.Reject.Reason != RejectOrderNotFound {
		t.Fatalf("unknown cancel must reject not-found, got %+v", cr3)
	}
}

func TestReplaceSamePriceKeepsPriority(t *testing.T) {
	// Account C holds exactly 5 shares of X, so growing its resting sell
	// beyond the original quantity must be rejected.
	positions := map[string]map[string]int64{
		"A": {"X": 0},
		"B": {"X": 100},
		"C": {"X": 5},
		"D": {"X": 100},
	}
	var err error
	e, err := NewEngine(EngineConfig{InitialCash: 1_000_000, InitialPositions: positions})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "B", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 5})
	mustSubmit(t, e, OrderRequest{ID: "s2", Account: "C", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 5})
	// Shrink s1 to 3; it must keep the head position at the level.
	rp, err := e.Replace("s1", 100, 3)
	if err != nil || rp.Reject != nil || !rp.KeptPriority {
		t.Fatalf("replace shrink: %v %+v", err, rp)
	}
	r := mustSubmit(t, e, OrderRequest{ID: "b1", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 4})
	if r.Trades[0].MakerOrderID != "s1" || r.Trades[0].Qty != 3 {
		t.Fatalf("amended s1 must retain priority x3, got %s x%d", r.Trades[0].MakerOrderID, r.Trades[0].Qty)
	}
	if r.Trades[1].MakerOrderID != "s2" || r.Trades[1].Qty != 1 {
		t.Fatalf("then s2 x1, got %+v", r.Trades[1:])
	}

	// Grow s2 (remaining 4) to 6: total sell would be 7 > the 5 shares C
	// holds, so the extra 2 shares are not available and it is rejected.
	if rp2, err := e.Replace("s2", 100, 6); err != nil || rp2.Reject == nil || rp2.Reject.Reason != RejectInsufficientShares {
		t.Fatalf("s2 grow without shares must reject INSUFFICIENT_SHARES, got %+v err=%v", rp2, err)
	}
}

func TestReplacePriceChange(t *testing.T) {
	e := newTestEngine(t, "A", "B")
	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "B", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 110, Qty: 5})
	mustSubmit(t, e, OrderRequest{ID: "b1", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 5})
	// Move bid to 110: it crosses and fills at the ask of 110 (0..5 qty).
	rp, err := e.Replace("b1", 110, 5)
	if err != nil || rp.Reject != nil {
		t.Fatalf("replace cross: %v %v", err, rp)
	}
	if rp.KeptPriority || len(rp.Trades) != 1 || rp.Trades[0].Price != 110 {
		t.Fatalf("expected fresh-priority crossing trade @110, got %+v", rp)
	}
}

func TestRejectedInstructionsLeaveNoTrace(t *testing.T) {
	e := newTestEngine(t, "A")
	_, books0, err := e.Snapshot("A")
	if err != nil {
		t.Fatal(err)
	}
	emptyLevels := func(bs map[string]BookSnapshot) int {
		n := 0
		for _, b := range bs {
			n += len(b.Bids) + len(b.Asks)
		}
		return n
	}
	cash0, _ := e.AccountSnapshot("A")

	mustReject(t, e, OrderRequest{ID: "x1", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 0, Qty: 5}, RejectInvalidPrice)
	mustReject(t, e, OrderRequest{ID: "x2", Account: "A", Symbol: "X", Side: Side(9), Type: OrderTypeLimit, Price: 10, Qty: 5}, RejectInvalidSide)
	mustReject(t, e, OrderRequest{ID: "x3", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 10, Qty: 0}, RejectInvalidQty)
	mustReject(t, e, OrderRequest{ID: "", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 10, Qty: 1}, RejectEmptyOrderID)
	mustReject(t, e, OrderRequest{ID: "x4", Account: "ghost", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 10, Qty: 1}, RejectUnknownAccount)
	// Insufficient funds: price 10 x qty 200000 = 2,000,000 > cash.
	mustReject(t, e, OrderRequest{ID: "x5", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 10, Qty: 200000}, RejectInsufficientFunds)

	cash1, _ := e.AccountSnapshot("A")
	_, books1, _ := e.Snapshot("A")
	if cash1.Cash != cash0.Cash || cash1.ReservedCash != cash0.ReservedCash || cash1.OpenOrders != 0 {
		t.Fatalf("account state changed after rejects: %+v vs %+v", cash0, cash1)
	}
	if emptyLevels(books0) != 0 || emptyLevels(books1) != 0 {
		t.Fatalf("books changed after rejects: %+v", books1)
	}
	// Rejected IDs are remembered and remain distinguishable.
	mustReject(t, e, OrderRequest{ID: "x5", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 10, Qty: 1}, RejectDuplicateOrderID)
}

func TestMarketOrderIOC(t *testing.T) {
	e := newTestEngine(t, "A", "B")
	// No liquidity -> rejected, not rested.
	mustReject(t, e, OrderRequest{ID: "m1", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeMarket, Qty: 5}, RejectNoLiquidity)
	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "B", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 5})
	// Partial market fill: takes 5 and finishes (no rest, no error).
	r := mustSubmit(t, e, OrderRequest{ID: "m2", Account: "A", Symbol: "X", Side: SideBuy, Type: OrderTypeMarket, Qty: 8})
	if r.Status != StatusCanceled || r.Trades[0].Qty != 5 {
		t.Fatalf("partial market should be IOC-canceled with 5 filled, got %s %+v", r.Status, r.Trades)
	}
}
