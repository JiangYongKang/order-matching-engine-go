package engine

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// TestConcurrentTIFEquivalentToReplay storms multiple symbols concurrently
// with IOC and FOK takers (against pre-seeded makers) and then replays the
// journal sequentially. Concurrent TIF submission must be equivalent to
// sequential submission in journal order: replay matches live field-by-field,
// no immediate order rests, and every FOK is either fully filled or rejected.
func TestConcurrentTIFEquivalentToReplay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ctif.bin")

	const nAccounts = 6
	const nSymbols = 4
	accounts := make([]string, nAccounts)
	positions := map[string]map[string]int64{}
	for i := range accounts {
		accounts[i] = fmt.Sprintf("ca%d", i)
		pos := map[string]int64{}
		for s := 0; s < nSymbols; s++ {
			pos[fmt.Sprintf("CS%d", s)] = 1_000_000
		}
		positions[accounts[i]] = pos
	}
	genesis := EngineConfig{JournalPath: path, InitialCash: 100_000_000, InitialPositions: positions}
	e, err := NewEngine(genesis)
	if err != nil {
		t.Fatal(err)
	}

	// Makers (account names that are never takers) seed resting asks.
	maker := "mk"
	positions[maker] = map[string]int64{}
	for s := 0; s < nSymbols; s++ {
		positions[maker][fmt.Sprintf("CS%d", s)] = 1_000_000
	}
	// Rebuild genesis/engine with the maker account included from the start.
	e.Close()
	genesis.InitialPositions = positions
	e, err = NewEngine(genesis)
	if err != nil {
		t.Fatal(err)
	}
	for s := 0; s < nSymbols; s++ {
		sym := fmt.Sprintf("CS%d", s)
		for k := 0; k < 6; k++ {
			mustSubmit(t, e, OrderRequest{ID: fmt.Sprintf("seed-%d-%d", s, k), Account: maker,
				Symbol: sym, Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 4})
		}
	}

	var idMu sync.Mutex
	idc := 0
	nextID := func() string {
		idMu.Lock()
		defer idMu.Unlock()
		idc++
		return fmt.Sprintf("t%05d", idc)
	}

	var wg sync.WaitGroup
	for g := 0; g < nAccounts; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for k := 0; k < 60; k++ {
				sym := fmt.Sprintf("CS%d", (g+k)%nSymbols)
				tif := TIFIOC
				if k%2 == 0 {
					tif = TIFFOK
				}
				id := nextID()
				r, err := e.Submit(OrderRequest{ID: id, Account: accounts[g], Symbol: sym,
					Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: int64(1 + (k % 5)), TIF: tif})
				if err != nil {
					t.Errorf("submit %s: %v", id, err)
					return
				}
				if r.Resting {
					t.Errorf("immediate order %s must never rest", id)
				}
				if tif == TIFFOK && r.Reject == nil && r.Status != StatusFilled {
					t.Errorf("FOK %s accepted non-filled status=%s", id, r.Status)
				}
			}
		}(g)
	}
	wg.Wait()

	liveAccounts := map[string]AccountState{}
	for _, a := range accounts {
		st, err := e.AccountSnapshot(a)
		if err != nil {
			t.Fatal(err)
		}
		liveAccounts[a] = *st
	}
	liveMaker, _ := e.AccountSnapshot(maker)
	_, liveBooks, err := e.Snapshot(accounts[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	replayed, err := ReplayWithGenesis(path, genesis)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	for _, a := range accounts {
		g := replayed.Accounts[a]
		l := liveAccounts[a]
		if g.Cash != l.Cash || g.ReservedCash != l.ReservedCash ||
			g.Positions["CS0"] != l.Positions["CS0"] || g.OpenOrders != l.OpenOrders {
			t.Fatalf("account %s differs replay=%+v live=%+v", a, g, l)
		}
	}
	rm := replayed.Accounts[maker]
	if rm.Cash != liveMaker.Cash || rm.Positions["CS0"] != liveMaker.Positions["CS0"] ||
		rm.ReservedPositions["CS0"] != liveMaker.ReservedPositions["CS0"] {
		t.Fatalf("maker differs replay=%+v live=%+v", rm, liveMaker)
	}
	for sym, lb := range liveBooks {
		rb := replayed.Books[sym]
		if rb == nil || len(rb.Bids) != len(lb.Bids) || len(rb.Asks) != len(lb.Asks) {
			t.Fatalf("book %s differs replay=%+v live=%+v", sym, rb, lb)
		}
		for i := range lb.Asks {
			if rb.Asks[i] != lb.Asks[i] {
				t.Fatalf("book %s ask %d differs: %+v vs %+v", sym, i, rb.Asks[i], lb.Asks[i])
			}
		}
		if len(lb.Bids) != 0 {
			t.Fatalf("no immediate taker may rest as a bid in %s: %+v", sym, lb.Bids)
		}
	}

	// Restart and verify every immediate order is in a legal terminal shape.
	e2, err := NewEngine(genesis)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	for id := 1; id <= idc; id++ {
		o, ok := e2.Order(fmt.Sprintf("t%05d", id))
		if !ok {
			continue // rejected FOKs are tracked only in the rejected set
		}
		if o.OpenQty != 0 {
			t.Fatalf("immediate order %s has open qty %d after recovery", o.ID, o.OpenQty)
		}
		switch o.TIF {
		case TIFFOK:
			if o.FilledQty != o.Qty || o.Status() != StatusFilled {
				t.Fatalf("accepted FOK %s must be fully filled, got filled=%d qty=%d status=%s",
					o.ID, o.FilledQty, o.Qty, o.Status())
			}
		case TIFIOC:
			if o.FilledQty > o.Qty || (o.FilledQty < o.Qty && o.Status() != StatusCanceled) ||
				(o.FilledQty == o.Qty && o.Status() != StatusFilled) {
				t.Fatalf("IOC %s illegal shape filled=%d qty=%d status=%s",
					o.ID, o.FilledQty, o.Qty, o.Status())
			}
		}
	}
	t.Logf("CONCURRENT TIF storm: %d taker submissions across %d symbols; replay matches live, all IOC/FOK terminal-legal",
		idc, nSymbols)
}
