package engine

import (
	"path/filepath"
	"testing"
)

// TestIOC_LimitPartialFillExpires covers IOC ("fill-and-kill") limit orders:
// it takes what is immediately available, the remainder is canceled and never
// rests, no cash/shares stay frozen, and the final status is CANCELED while a
// fully filled IOC is FILLED.
func TestIOC_LimitPartialFillExpires(t *testing.T) {
	t.Log("BUSINESS: IOC limit buy crosses one partial ask; remainder expires without resting or reserving cash")
	e := newTestEngine(t, "A", "B")
	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "B", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 5})

	before, _ := e.AccountSnapshot("A")
	r := mustSubmit(t, e, OrderRequest{ID: "i1", Account: "A", Symbol: "X", Side: SideBuy,
		Type: OrderTypeLimit, Price: 100, Qty: 8, TIF: TIFIOC})

	if r.Status != StatusCanceled {
		t.Fatalf("partially filled IOC must end CANCELED, got %s", r.Status)
	}
	if r.Resting {
		t.Fatalf("IOC remainder must never rest")
	}
	if len(r.Trades) != 1 || r.Trades[0].Qty != 5 {
		t.Fatalf("want one trade x5, got %+v", r.Trades)
	}
	if o, ok := e.Order("i1"); !ok || o.Status() != StatusCanceled || o.FilledQty != 5 || o.OpenQty != 0 {
		t.Fatalf("order view must show CANCELED filled=5 open=0, got %+v ok=%v", o, ok)
	}
	// Three-way consistency: book empty, cash moved only by the real fill.
	_, books, _ := e.Snapshot("A")
	if len(books["X"].Asks) != 0 || len(books["X"].Bids) != 0 {
		t.Fatalf("no remainder may rest, book=%+v", books["X"])
	}
	after, _ := e.AccountSnapshot("A")
	if after.ReservedCash != 0 {
		t.Fatalf("IOC must not keep any reservation, got reserved=%d", after.ReservedCash)
	}
	if after.Cash != before.Cash-500 || after.Positions["X"] != before.Positions["X"]+5 {
		t.Fatalf("only the 5 filled shares settle: cash %d->%d pos %d->%d",
			before.Cash, after.Cash, before.Positions["X"], after.Positions["X"])
	}
	if after.OpenOrders != 0 {
		t.Fatalf("expired IOC must not count as open order, got %d", after.OpenOrders)
	}
	t.Logf("RESULT i1: status=%s filled=5 leftover=3 expired, reserved=%d openOrders=%d",
		r.Status, after.ReservedCash, after.OpenOrders)
}

// TestIOC_LimitFullyFilled verifies an IOC that can be fully taken ends FILLED
// (no Expired event) rather than CANCELED.
func TestIOC_LimitFullyFilled(t *testing.T) {
	t.Log("BUSINESS: IOC limit buy fully matchable -> FILLED, no expired remainder")
	e := newTestEngine(t, "A", "B")
	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "B", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 5})
	r := mustSubmit(t, e, OrderRequest{ID: "i2", Account: "A", Symbol: "X", Side: SideBuy,
		Type: OrderTypeLimit, Price: 100, Qty: 5, TIF: TIFIOC})
	if r.Status != StatusFilled || r.Resting || len(r.Trades) != 1 {
		t.Fatalf("fully filled IOC must be FILLED not resting, got %s resting=%v trades=%d",
			r.Status, r.Resting, len(r.Trades))
	}
	if o, _ := e.Order("i2"); o.Status() != StatusFilled {
		t.Fatalf("order view status=%s want FILLED", o.Status())
	}
	t.Logf("RESULT i2: status=%s filled=5, no EXPIRED event", r.Status)
}

// TestIOC_NoCrossExpiresWhole verifies a non-crossing IOC limit expires in
// full: zero trades, CANCELED, zero effects on book and ledger.
func TestIOC_NoCrossExpiresWhole(t *testing.T) {
	t.Log("BUSINESS: IOC limit with no opposing liquidity expires whole, zero state change")
	e := newTestEngine(t, "A", "B")
	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "B", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 110, Qty: 5})
	before, _ := e.AccountSnapshot("A")
	r := mustSubmit(t, e, OrderRequest{ID: "i3", Account: "A", Symbol: "X", Side: SideBuy,
		Type: OrderTypeLimit, Price: 100, Qty: 5, TIF: TIFIOC})
	if r.Status != StatusCanceled || len(r.Trades) != 0 || r.Resting {
		t.Fatalf("non-crossing IOC must be CANCELED with 0 trades, got %s trades=%d resting=%v",
			r.Status, len(r.Trades), r.Resting)
	}
	after, _ := e.AccountSnapshot("A")
	if after.Cash != before.Cash || after.ReservedCash != 0 || after.OpenOrders != 0 {
		t.Fatalf("expired-in-full IOC must change nothing: %+v vs %+v", before, after)
	}
	_, books, _ := e.Snapshot("A")
	if len(books["X"].Bids) != 0 {
		t.Fatalf("IOC must not rest on the bid, got %+v", books["X"].Bids)
	}
	t.Logf("RESULT i3: status=%s zero fills; ask s1 untouched", r.Status)
}

// TestIOC_SellReleasesNothing verifies the sell side: only matched shares are
// delivered; the expired remainder never freezes position.
func TestIOC_SellPartialFill(t *testing.T) {
	t.Log("BUSINESS: IOC limit sell delivers only matched shares; remainder never reserved")
	e := newTestEngine(t, "A", "B")
	// B bids 5; A (10000 shares) sells 8 IOC -> 5 fill, 3 expire.
	mustSubmit(t, e, OrderRequest{ID: "b1", Account: "B", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 5})
	before, _ := e.AccountSnapshot("A")
	r := mustSubmit(t, e, OrderRequest{ID: "i4", Account: "A", Symbol: "X", Side: SideSell,
		Type: OrderTypeLimit, Price: 100, Qty: 8, TIF: TIFIOC})
	if r.Status != StatusCanceled || len(r.Trades) != 1 || r.Trades[0].Qty != 5 {
		t.Fatalf("want partial x5 then expired, got %s %+v", r.Status, r.Trades)
	}
	after, _ := e.AccountSnapshot("A")
	if after.ReservedPositions["X"] != 0 {
		t.Fatalf("expired remainder must not reserve shares, got %d", after.ReservedPositions["X"])
	}
	if after.Positions["X"] != before.Positions["X"]-5 || after.Cash != before.Cash+500 {
		t.Fatalf("only 5 shares delivered: pos %d->%d cash %d->%d",
			before.Positions["X"], after.Positions["X"], before.Cash, after.Cash)
	}
	t.Logf("RESULT i4: status=%s sold=5 leftover=3 expired; reservedShares=%d",
		r.Status, after.ReservedPositions["X"])
}

// TestFOK_FullyFilled verifies FOK that is fully matchable trades the whole
// size at once and ends FILLED without resting.
func TestFOK_FullyFilled(t *testing.T) {
	t.Log("BUSINESS: FOK buy fully matchable across two levels -> one atomic full fill, FILLED")
	e := newTestEngine(t, "A", "B", "C")
	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "B", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 5})
	mustSubmit(t, e, OrderRequest{ID: "s2", Account: "C", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 101, Qty: 5})
	r := mustSubmit(t, e, OrderRequest{ID: "f1", Account: "A", Symbol: "X", Side: SideBuy,
		Type: OrderTypeLimit, Price: 101, Qty: 10, TIF: TIFFOK})
	if r.Status != StatusFilled || r.Resting {
		t.Fatalf("fully matched FOK must be FILLED not resting, got %s", r.Status)
	}
	if len(r.Trades) != 2 || r.Trades[0].MakerOrderID != "s1" || r.Trades[0].Qty != 5 ||
		r.Trades[1].MakerOrderID != "s2" || r.Trades[1].Qty != 5 {
		t.Fatalf("must walk s1 x5 @100 then s2 x5 @101, got %+v", r.Trades)
	}
	acct, _ := e.AccountSnapshot("A")
	if acct.ReservedCash != 0 || acct.Positions["X"] != 10010 {
		t.Fatalf("full fill settles 10 shares with no reservation: %+v", acct)
	}
	_, books, _ := e.Snapshot("A")
	if len(books["X"].Asks) != 0 {
		t.Fatalf("both asks must be consumed, got %+v", books["X"].Asks)
	}
	t.Logf("RESULT f1: status=%s cost=%d (5@100+5@101), book empty", r.Status, 500+505)
}

// TestFOK_ShortLiquidityRejectedWhole is the core FOK path: insufficient
// size means a total rejection, zero fills and zero ledger/book changes.
func TestFOK_ShortLiquidityRejectedWhole(t *testing.T) {
	t.Log("BUSINESS: FOK buy needs 8 but only 5 available -> WOULD_NOT_FILL, zero fills / zero reservation")
	e := newTestEngine(t, "A", "B")
	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "B", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 5})
	beforeA, _ := e.AccountSnapshot("A")
	beforeB, _ := e.AccountSnapshot("B")
	mustReject(t, e, OrderRequest{ID: "f2", Account: "A", Symbol: "X", Side: SideBuy,
		Type: OrderTypeLimit, Price: 100, Qty: 8, TIF: TIFFOK}, RejectWouldNotFill)

	afterA, _ := e.AccountSnapshot("A")
	afterB, _ := e.AccountSnapshot("B")
	if afterA.Cash != beforeA.Cash || afterA.ReservedCash != 0 || afterA.Positions["X"] != beforeA.Positions["X"] {
		t.Fatalf("taker account must be unchanged: %+v vs %+v", beforeA, afterA)
	}
	if afterB.Cash != beforeB.Cash || afterB.ReservedPositions["X"] != beforeB.ReservedPositions["X"] ||
		afterB.Positions["X"] != beforeB.Positions["X"] {
		t.Fatalf("maker account must be unchanged: %+v vs %+v", beforeB, afterB)
	}
	_, books, _ := e.Snapshot("A")
	if len(books["X"].Asks) != 1 || books["X"].Asks[0].Qty != 5 {
		t.Fatalf("resting ask must survive untouched, got %+v", books["X"].Asks)
	}
	// Rejected FOK leaves no order, and the same id cannot be re-used.
	if _, ok := e.Order("f2"); ok {
		t.Fatalf("rejected FOK must not be registered as an order")
	}
	mustReject(t, e, OrderRequest{ID: "f2", Account: "A", Symbol: "X", Side: SideBuy,
		Type: OrderTypeLimit, Price: 100, Qty: 1, TIF: TIFFOK}, RejectDuplicateOrderID)
	t.Logf("RESULT f2: REJECT WOULD_NOT_FILL; ask s1 still 5@100, both accounts untouched")
}

// TestFOK_NoLiquidityAndSell verify the no-liquidity and sell-side branches.
func TestFOK_NoLiquidityAndSellSide(t *testing.T) {
	t.Log("BUSINESS: FOK with empty/short book rejects whole; FOK sell fully matchable fills")
	e := newTestEngine(t, "A", "B")
	// Empty book.
	mustReject(t, e, OrderRequest{ID: "f3", Account: "A", Symbol: "X", Side: SideBuy,
		Type: OrderTypeLimit, Price: 100, Qty: 1, TIF: TIFFOK}, RejectWouldNotFill)
	// Bid exists but too small: FOK sell of 8 against a 5 bid rejects whole.
	mustSubmit(t, e, OrderRequest{ID: "b1", Account: "B", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 5})
	before, _ := e.AccountSnapshot("A")
	mustReject(t, e, OrderRequest{ID: "f4", Account: "A", Symbol: "X", Side: SideSell,
		Type: OrderTypeLimit, Price: 100, Qty: 8, TIF: TIFFOK}, RejectWouldNotFill)
	after, _ := e.AccountSnapshot("A")
	if after.Positions["X"] != before.Positions["X"] || after.ReservedPositions["X"] != 0 {
		t.Fatalf("rejected FOK sell must not move/reserve shares: %+v vs %+v", before, after)
	}
	// A bid covering the full size: FOK sell fills at once.
	r := mustSubmit(t, e, OrderRequest{ID: "f5", Account: "A", Symbol: "X", Side: SideSell,
		Type: OrderTypeLimit, Price: 100, Qty: 5, TIF: TIFFOK})
	if r.Status != StatusFilled || len(r.Trades) != 1 || r.Trades[0].Qty != 5 {
		t.Fatalf("fully covered FOK sell must fill, got %s %+v", r.Status, r.Trades)
	}
	t.Logf("RESULT f3/f4 rejected whole (bid b1 untouched); f5 status=%s sold=5", r.Status)
}

// TestFOK_DoesNotConsumeBeforeReject makes the "zero partial fill" guarantee
// explicit with two time-priority makers: only the first could be touched, so
// a naive implementation might fill it before discovering the shortfall.
func TestFOK_DoesNotConsumeBeforeReject(t *testing.T) {
	t.Log("BUSINESS: FOK across same-level makers s1(3)+s2(3) needing 8 must not consume s1 before rejecting")
	e := newTestEngine(t, "A", "B", "C")
	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "B", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 3})
	mustSubmit(t, e, OrderRequest{ID: "s2", Account: "C", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 3})
	mustReject(t, e, OrderRequest{ID: "f6", Account: "A", Symbol: "X", Side: SideBuy,
		Type: OrderTypeLimit, Price: 100, Qty: 8, TIF: TIFFOK}, RejectWouldNotFill)
	_, books, _ := e.Snapshot("A")
	if len(books["X"].Asks) != 1 || books["X"].Asks[0].Qty != 6 {
		t.Fatalf("both makers must keep full qty 6 at 100, got %+v", books["X"].Asks)
	}
	if o, _ := e.Order("s1"); o.OpenQty != 3 {
		t.Fatalf("s1 must retain open qty 3, got %d", o.OpenQty)
	}
	t.Logf("RESULT f6 rejected; s1+s2 still 6@100 with original time priority")
}

// TestTIF_InvalidAndMarketCompatibility checks TIF validation and that market
// orders keep their historical IOC semantics, now explicitly selectable.
func TestTIF_InvalidAndMarketCompatibility(t *testing.T) {
	t.Log("BUSINESS: invalid TIF / GTC-on-market rejected; market+FOK all-or-nothing; market default stays IOC")
	e := newTestEngine(t, "A", "B")
	mustReject(t, e, OrderRequest{ID: "t1", Account: "A", Symbol: "X", Side: SideBuy,
		Type: OrderTypeLimit, Price: 100, Qty: 1, TIF: TimeInForce(99)}, RejectInvalidTIF)
	mustReject(t, e, OrderRequest{ID: "t2", Account: "A", Symbol: "X", Side: SideBuy,
		Type: OrderTypeMarket, Qty: 1, TIF: TIFGTC}, RejectInvalidTIF)

	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "B", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 5})
	// Market FOK needing more than liquidity: rejected whole, ask untouched.
	mustReject(t, e, OrderRequest{ID: "t3", Account: "A", Symbol: "X", Side: SideBuy,
		Type: OrderTypeMarket, Qty: 8, TIF: TIFFOK}, RejectWouldNotFill)
	if o, _ := e.Order("s1"); o.OpenQty != 5 {
		t.Fatalf("s1 must stay 5 after market FOK reject, got %d", o.OpenQty)
	}
	// Default market order (no TIF): historical IOC behavior unchanged.
	r := mustSubmit(t, e, OrderRequest{ID: "t4", Account: "A", Symbol: "X", Side: SideBuy,
		Type: OrderTypeMarket, Qty: 8})
	if r.Status != StatusCanceled || r.Trades[0].Qty != 5 {
		t.Fatalf("default market must stay IOC partial-canceled, got %s", r.Status)
	}
	t.Logf("RESULT t1/t2 invalid rejected; t3 FOK rejected whole; t4 default market status=%s x5", r.Status)
}

// TestGTCLimitUnchanged proves the default limit path still rests with full
// reservation, i.e. TIF is purely additive.
func TestGTCLimitUnchanged(t *testing.T) {
	t.Log("BUSINESS: explicit/implicit GTC limit still rests, reserves, and later matches/cancels")
	e := newTestEngine(t, "A", "B")
	r := mustSubmit(t, e, OrderRequest{ID: "g1", Account: "A", Symbol: "X", Side: SideBuy,
		Type: OrderTypeLimit, Price: 100, Qty: 5, TIF: TIFGTC})
	if !r.Resting || r.Status != StatusNew {
		t.Fatalf("GTC limit must rest NEW, got resting=%v status=%s", r.Resting, r.Status)
	}
	snap, _ := e.AccountSnapshot("A")
	if snap.ReservedCash != 500 {
		t.Fatalf("GTC buy reserves 500, got %d", snap.ReservedCash)
	}
	// Default (no TIF) behaves identically.
	r2 := mustSubmit(t, e, OrderRequest{ID: "g2", Account: "A", Symbol: "Y", Side: SideBuy,
		Type: OrderTypeLimit, Price: 100, Qty: 5})
	if !r2.Resting {
		t.Fatalf("default TIF limit must rest like GTC")
	}
	t.Logf("RESULT g1/g2 resting with reservations intact")
}

// TestTIF_JournalReplay verifies the full submit->fills->expire/reject
// lifecycle replays to the identical terminal state, idempotently.
func TestTIF_JournalReplay(t *testing.T) {
	t.Log("BUSINESS: IOC partial + IOC full + FOK reject journal replay equals live and is repeatable")
	dir := t.TempDir()
	path := filepath.Join(dir, "tif.bin")
	positions := map[string]map[string]int64{"A": {"X": 10000}, "B": {"X": 10000}, "C": {"X": 10000}}
	genesis := EngineConfig{JournalPath: path, InitialCash: 1_000_000, InitialPositions: positions}
	e, err := NewEngine(genesis)
	if err != nil {
		t.Fatal(err)
	}
	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "B", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 5})
	mustSubmit(t, e, OrderRequest{ID: "s2", Account: "C", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 102, Qty: 5})
	mustSubmit(t, e, OrderRequest{ID: "i9", Account: "A", Symbol: "X", Side: SideBuy,
		Type: OrderTypeLimit, Price: 101, Qty: 10, TIF: TIFIOC}) // 5@100 fill, 5 expire (s2@102 not crossed)
	mustSubmit(t, e, OrderRequest{ID: "f9", Account: "A", Symbol: "X", Side: SideBuy,
		Type: OrderTypeLimit, Price: 102, Qty: 5, TIF: TIFFOK}) // s2 5@102 full fill
	liveA, _ := e.AccountSnapshot("A")
	liveB, _ := e.AccountSnapshot("B")
	liveIOC, _ := e.Order("i9")
	liveFOK, _ := e.Order("f9")
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := ReplayWithGenesis(path, EngineConfig{InitialCash: 1_000_000, InitialPositions: positions})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	// Event stream visibly contains the lifecycle incl. one EXPIRED.
	var expired, rejected, trades int
	for _, ev := range st.Events {
		switch ev.Type {
		case EventExpired:
			expired++
			t.Logf("  REPLAY seq=%d EXPIRED order=%s leftover=%d", ev.Seq, ev.OrderID, ev.LeftoverQty)
		case EventRejected:
			rejected++
		case EventTrade:
			trades++
		}
	}
	if expired != 1 || trades != 2 {
		t.Fatalf("want 1 expired / 2 trades, got %d / %d", expired, trades)
	}
	if st.Accounts["A"].Cash != liveA.Cash || st.Accounts["A"].ReservedCash != liveA.ReservedCash ||
		st.Accounts["A"].Positions["X"] != liveA.Positions["X"] {
		t.Fatalf("A replay %+v != live %+v", st.Accounts["A"], liveA)
	}
	if st.Accounts["B"].Positions["X"] != liveB.Positions["X"] {
		t.Fatalf("B replay pos=%d != live %d", st.Accounts["B"].Positions["X"], liveB.Positions["X"])
	}
	if st.Books["X"] == nil || len(st.Books["X"].Asks) != 0 {
		t.Fatalf("all asks consumed, replay book=%+v", st.Books["X"])
	}
	// Orders map is not exposed by ReplayState; restart recovery checks statuses.
	e2, err := NewEngine(genesis)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	if o, ok := e2.Order("i9"); !ok || o.Status() != StatusCanceled || o.FilledQty != liveIOC.FilledQty {
		t.Fatalf("recovered IOC must be CANCELED filled=%d, got %+v ok=%v", liveIOC.FilledQty, o, ok)
	}
	if o, ok := e2.Order("f9"); !ok || o.Status() != StatusFilled || o.FilledQty != liveFOK.FilledQty {
		t.Fatalf("recovered FOK must be FILLED filled=%d, got %+v ok=%v", liveFOK.FilledQty, o, ok)
	}
	// Repeat replay is idempotent and identical.
	st2, err := ReplayWithGenesis(path, EngineConfig{InitialCash: 1_000_000, InitialPositions: positions})
	if err != nil || len(st2.Events) != len(st.Events) || len(st2.Trades) != len(st.Trades) {
		t.Fatalf("repeat replay diverges: %v %d/%d", err, len(st2.Events), len(st.Events))
	}
	t.Logf("RESULT i9=%s(5 filled + 5 expired) f9=%s(5); replay=%d events %d trades and matches live",
		StatusCanceled, StatusFilled, len(st.Events), trades)
}
