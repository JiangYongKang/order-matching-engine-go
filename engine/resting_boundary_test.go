package engine

import "testing"

// TestLimitMakerPartialKeepsResting is the dedicated boundary test for the
// ordinary (GTC) limit flow that must not regress behind the new time-in-force
// work: when a resting maker is only partially consumed,
//
//  1. the leftover stays at the SAME price with its time priority;
//  2. it keeps trading against later crossing orders;
//  3. it can instead be canceled, releasing the still-frozen funds/shares;
//
// and order status, order book and account reservations stay mutually
// consistent at every step (not just at full-fill / no-fill extremes).
func TestLimitMakerPartialKeepsResting(t *testing.T) {
	t.Log("BUSINESS: GTC maker partially filled keeps resting at its level, then fills the rest; order/book/account reconciled")
	e := newTestEngine(t, "A", "B", "C")

	// B rests a 10-lot sell at 100 (10 shares frozen).
	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "B", Symbol: "X",
		Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 10})
	baseB, _ := e.AccountSnapshot("B")
	if baseB.ReservedPositions["X"] != 10 {
		t.Fatalf("10 shares must be frozen at submit, got %d", baseB.ReservedPositions["X"])
	}

	// A crosses only 6: maker s1 fills 6 and keeps 4 resting at 100.
	r := mustSubmit(t, e, OrderRequest{ID: "b1", Account: "A", Symbol: "X",
		Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 6})
	if len(r.Trades) != 1 || r.Trades[0].MakerOrderID != "s1" || r.Trades[0].Qty != 6 {
		t.Fatalf("b1 must take exactly 6 from s1, got %+v", r.Trades)
	}

	// (1) order view
	s1, ok := e.Order("s1")
	if !ok {
		t.Fatal("s1 missing")
	}
	if s1.Status() != StatusPartiallyFilled || s1.FilledQty != 6 || s1.OpenQty != 4 || s1.Price != 100 {
		t.Fatalf("s1 must be PARTIALLY_FILLED 6/4 still @100, got status=%s filled=%d open=%d price=%d",
			s1.Status(), s1.FilledQty, s1.OpenQty, s1.Price)
	}
	// (2) order book: exactly 4 remain on the ask at 100, same level.
	_, books, _ := e.Snapshot("A")
	if len(books["X"].Asks) != 1 || books["X"].Asks[0].Price != 100 || books["X"].Asks[0].Qty != 4 {
		t.Fatalf("ask must show 4@100 after partial fill, got %+v", books["X"].Asks)
	}
	// (3) account: 6 settled, 4 still frozen, cash from only the 6 fills.
	afterB, _ := e.AccountSnapshot("B")
	if afterB.Positions["X"] != baseB.Positions["X"]-6 {
		t.Fatalf("B must have delivered 6 shares, pos %d->%d", baseB.Positions["X"], afterB.Positions["X"])
	}
	if afterB.ReservedPositions["X"] != 4 {
		t.Fatalf("exactly 4 shares must remain frozen, got %d", afterB.ReservedPositions["X"])
	}
	if afterB.Cash != baseB.Cash+600 {
		t.Fatalf("B cash must rise by only 600, got +%d", afterB.Cash-baseB.Cash)
	}
	if afterB.OpenOrders != 1 {
		t.Fatalf("partially filled maker still counts as one open order, got %d", afterB.OpenOrders)
	}
	t.Logf("STEP1 s1 partial: status=%s filled=6 open=4; ask=4@100; reservedShares=4 cash+600",
		s1.Status())

	// C crosses the remaining 4: the leftover (still head of the level) fills.
	r2 := mustSubmit(t, e, OrderRequest{ID: "b2", Account: "C", Symbol: "X",
		Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 4})
	if len(r2.Trades) != 1 || r2.Trades[0].MakerOrderID != "s1" || r2.Trades[0].Qty != 4 {
		t.Fatalf("b2 must finish s1 with 4, got %+v", r2.Trades)
	}
	s1, _ = e.Order("s1")
	if s1.Status() != StatusFilled || s1.OpenQty != 0 || s1.FilledQty != 10 {
		t.Fatalf("s1 must be FILLED 10/0, got %s filled=%d open=%d", s1.Status(), s1.FilledQty, s1.OpenQty)
	}
	_, books, _ = e.Snapshot("A")
	if len(books["X"].Asks) != 0 {
		t.Fatalf("ask level must be gone after the remainder fills, got %+v", books["X"].Asks)
	}
	endB, _ := e.AccountSnapshot("B")
	if endB.ReservedPositions["X"] != 0 || endB.Positions["X"] != baseB.Positions["X"]-10 ||
		endB.Cash != baseB.Cash+1000 || endB.OpenOrders != 0 {
		t.Fatalf("post-full-fill three-way mismatch: %+v", endB)
	}
	t.Logf("STEP2 s1 complete: status=%s filled=10; book empty; reserved=0 cash+1000", s1.Status())
}

// TestLimitMakerPartialThenCancel covers the other branch of the boundary: the
// resting remainder is canceled (instead of filled), releasing exactly the
// still-frozen remainder and leaving prior fills intact.
func TestLimitMakerPartialThenCancel(t *testing.T) {
	t.Log("BUSINESS: GTC maker partially filled then canceled: remainder released, fills stay, three-way consistent")
	e := newTestEngine(t, "A", "B")

	mustSubmit(t, e, OrderRequest{ID: "s2", Account: "B", Symbol: "X",
		Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 10})
	baseB, _ := e.AccountSnapshot("B")
	mustSubmit(t, e, OrderRequest{ID: "b1", Account: "A", Symbol: "X",
		Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 6})

	s2, _ := e.Order("s2")
	if s2.Status() != StatusPartiallyFilled || s2.OpenQty != 4 {
		t.Fatalf("pre-cancel s2 must be partial 6/4, got %s %d/%d", s2.Status(), s2.FilledQty, s2.OpenQty)
	}
	cr, err := e.Cancel("s2")
	if err != nil || cr.Reject != nil {
		t.Fatalf("cancel remainder: %v %+v", err, cr)
	}
	// order
	s2, _ = e.Order("s2")
	if s2.Status() != StatusCanceled || s2.OpenQty != 0 || s2.FilledQty != 6 {
		t.Fatalf("canceled partial maker must be CANCELED 6/0, got %s %d/%d", s2.Status(), s2.FilledQty, s2.OpenQty)
	}
	// book
	_, books, _ := e.Snapshot("A")
	if len(books["X"].Asks) != 0 {
		t.Fatalf("cancel must remove the 4-lot remainder, got %+v", books["X"].Asks)
	}
	// account: 6 delivered, 4 released (not delivered), reservation back to 0
	endB, _ := e.AccountSnapshot("B")
	if endB.Positions["X"] != baseB.Positions["X"]-6 ||
		endB.ReservedPositions["X"] != 0 ||
		endB.Cash != baseB.Cash+600 || endB.OpenOrders != 0 {
		t.Fatalf("post-cancel three-way mismatch: %+v", endB)
	}
	// already-canceled remainder cannot be canceled again
	if cr2, _ := e.Cancel("s2"); cr2.Reject == nil || cr2.Reject.Reason != RejectOrderClosed {
		t.Fatalf("second cancel must be ORDER_NOT_ACTIVE, got %+v", cr2)
	}
	t.Logf("RESULT s2: 6 filled then 4 canceled; pos-6 reserved=0 cash+600 openOrders=0")
}
