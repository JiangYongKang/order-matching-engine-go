package engine

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// TestCancelAmendRace drives one heavily contested order: one goroutine tries
// to fill it, another cancels it, a third amends it, repeatedly. Whatever
// happens, the final state must equal a sequential replay of the journal and
// the order must never be filled twice nor left in a partially-effected state.
func TestCancelAmendRace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "race.bin")
	genesis := EngineConfig{
		JournalPath: path,
		InitialCash: 100_000_000,
		InitialPositions: map[string]map[string]int64{
			"maker":  {"X": 1_000_000},
			"taker":  {"X": 0},
			"editor": {"X": 1_000_000},
		},
	}
	e, err := NewEngine(genesis)
	if err != nil {
		t.Fatal(err)
	}

	const rounds = 200
	var wg sync.WaitGroup
	decision := func(rj *RejectError) string {
		if rj == nil {
			return "ok"
		}
		return string(rj.Reason)
	}
	for i := 0; i < rounds; i++ {
		oid := fmt.Sprintf("m%04d", i)
		mr, err := e.Submit(OrderRequest{ID: oid, Account: "maker", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100 + int64(i%5), Qty: 10})
		if err != nil || mr.Reject != nil {
			t.Fatalf("seed %s: %v %v", oid, err, mr.Reject)
		}
		start := make(chan struct{})
		var once sync.WaitGroup
		once.Add(3)
		reasons := [3]string{}
		wg.Add(3)
		// Filler: buys the full quantity.
		go func() {
			defer wg.Done()
			once.Done()
			<-start
			r, err := e.Submit(OrderRequest{ID: fmt.Sprintf("t%04d", i), Account: "taker", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 200, Qty: 10})
			if err != nil {
				t.Errorf("fill submit: %v", err)
				return
			}
			reasons[0] = decision(r.Reject)
		}()
		// Canceller.
		go func() {
			defer wg.Done()
			once.Done()
			<-start
			cr, err := e.Cancel(oid)
			if err != nil {
				t.Errorf("cancel: %v", err)
				return
			}
			reasons[1] = decision(cr.Reject)
		}()
		// Editor: same-price shrink (keeps priority).
		go func() {
			defer wg.Done()
			once.Done()
			<-start
			rp, err := e.Replace(oid, 100+int64(i%5), 8)
			if err != nil {
				t.Errorf("replace: %v", err)
				return
			}
			reasons[2] = decision(rp.Reject)
		}()
		once.Wait()
		close(start)
		wg.Wait()
		t.Logf("round %d: fill=%s cancel=%s amend=%s", i, reasons[0], reasons[1], reasons[2])
	}

	// Capture live invariant: conservation of cash + notional-free checks.
	taker, _ := e.AccountSnapshot("taker")
	maker, _ := e.AccountSnapshot("maker")
	_, books, err := e.Snapshot("maker")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := ReplayWithGenesis(path, genesis)
	if err != nil {
		t.Fatal(err)
	}
	rt, rm := st.Accounts["taker"], st.Accounts["maker"]
	if rt.Cash != taker.Cash || rt.Positions["X"] != taker.Positions["X"] || rt.ReservedCash != taker.ReservedCash {
		t.Fatalf("taker replay/live differ: %+v vs %+v", rt, taker)
	}
	if rm.Cash != maker.Cash || rm.Positions["X"] != maker.Positions["X"] ||
		rm.ReservedPositions["X"] != maker.ReservedPositions["X"] || rm.OpenOrders != maker.OpenOrders {
		t.Fatalf("maker replay/live differ: %+v vs %+v", rm, maker)
	}
	// Every resting sell quantity in the live book must match replay exactly.
	lb := books["X"]
	rb := st.Books["X"]
	if len(lb.Asks) != len(rb.Asks) {
		t.Fatalf("ask depth replay/live differ: %d vs %d", len(rb.Asks), len(lb.Asks))
	}
	for i := range lb.Asks {
		if lb.Asks[i] != rb.Asks[i] {
			t.Fatalf("ask level %d replay/live differ: %+v vs %+v", i, rb.Asks[i], lb.Asks[i])
		}
	}
	// Global conservation: total cash constant (trades only move cash between
	// the two parties, reservations are not cash destruction).
	if rt.Cash+rm.Cash+st.Accounts["editor"].Cash != 100_000_000*3 {
		t.Fatalf("cash not conserved: taker=%d maker=%d editor=%d", rt.Cash, rm.Cash, st.Accounts["editor"].Cash)
	}
	t.Logf("RACE resolved: trades=%d takerPos=%d makerPos=%d asks=%d",
		len(st.Trades), rt.Positions["X"], rm.Positions["X"], len(rb.Asks))
}
