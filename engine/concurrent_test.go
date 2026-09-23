package engine

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// TestConcurrentMultiSymbolEquivalentToReplay submits many orders across
// multiple symbols from many goroutines, then asserts that the recovered
// state (from the journal, replayed sequentially) exactly matches the live
// state. Since journal order defines one total order, concurrent execution
// is proven equivalent to sequential execution in that order.
func TestConcurrentMultiSymbolEquivalentToReplay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.bin")

	const nAccounts = 8
	const nSymbols = 6
	const perGoroutine = 40
	accounts := make([]string, nAccounts)
	positions := map[string]map[string]int64{}
	for i := range accounts {
		accounts[i] = fmt.Sprintf("acct%d", i)
		pos := map[string]int64{}
		for s := 0; s < nSymbols; s++ {
			pos[fmt.Sprintf("SYM%d", s)] = 1_000_000
		}
		positions[accounts[i]] = pos
	}
	genesis := EngineConfig{JournalPath: path, InitialCash: 100_000_000, InitialPositions: positions}
	e, err := NewEngine(genesis)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var idMu sync.Mutex
	idCounter := 0
	nextID := func() string {
		idMu.Lock()
		defer idMu.Unlock()
		idCounter++
		return fmt.Sprintf("o%05d", idCounter)
	}

	// Two kinds of workers: market makers place two-sided quotes; traders
	// aggressively cross (and some cancel/amend the orders they created).
	var madeMu sync.Mutex
	var made []string
	var gWG sync.WaitGroup
	for g := 0; g < nAccounts; g++ {
		gWG.Add(1)
		go func(g int) {
			defer gWG.Done()
			acct := accounts[g]
			for s := 0; s < nSymbols; s++ {
				sym := fmt.Sprintf("SYM%d", s)
				for k := 0; k < 5; k++ {
					id := nextID()
					price := int64(90 + ((g + k + s) % 10))
					r, err := e.Submit(OrderRequest{ID: id, Account: acct, Symbol: sym, Side: SideSell, Type: OrderTypeLimit, Price: price, Qty: 3})
					if err != nil {
						t.Errorf("submit: %v", err)
						return
					}
					if r != nil && r.Reject == nil && r.Resting {
						madeMu.Lock()
						made = append(made, id)
						madeMu.Unlock()
					}
				}
			}
		}(g)
	}
	gWG.Wait()
	t.Logf("seeded %d resting sells before concurrent storm", len(made))

	for g := 0; g < nAccounts; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			local := make([]string, 0)
			for k := 0; k < perGoroutine; k++ {
				sym := fmt.Sprintf("SYM%d", (g+k)%nSymbols)
				acct := accounts[g]
				price := int64(90 + (k % 15))
				id := nextID()
				side := SideBuy
				if k%4 == 0 {
					side = SideSell
				}
				r, err := e.Submit(OrderRequest{ID: id, Account: acct, Symbol: sym, Side: side, Type: OrderTypeLimit, Price: price, Qty: 2})
				if err != nil {
					t.Errorf("submit %s: %v", id, err)
					return
				}
				if r != nil && r.Resting {
					local = append(local, id)
				}
				// Concurrent cancel/amend racing on this goroutine's own orders.
				if len(local) > 0 && k%3 == 0 {
					target := local[len(local)-1]
					if k%2 == 0 {
						if _, err := e.Cancel(target); err != nil {
							t.Errorf("cancel %s: %v", target, err)
						}
						local = local[:len(local)-1]
					} else {
						if rp, err := e.Replace(target, price, 1); err != nil {
							t.Errorf("replace %s: %v", target, err)
						} else if rp != nil && rp.Reject != nil &&
							rp.Reject.Reason != RejectOrderClosed && rp.Reject.Reason != RejectOrderNotFound &&
							rp.Reject.Reason != RejectInsufficientFunds && rp.Reject.Reason != RejectInsufficientShares &&
							rp.Reject.Reason != RejectSelfTrade && rp.Reject.Reason != RejectRiskGrossExposure &&
							rp.Reject.Reason != RejectRiskSymbolPosition && rp.Reject.Reason != RejectRiskGlobalExposure {
							t.Errorf("unexpected replace reject for %s: %v", target, rp.Reject)
						}
					}
				}
			}
		}(g)
	}
	wg.Wait()

	// Live reference snapshots.
	liveAccounts := map[string]AccountState{}
	for _, a := range accounts {
		st, err := e.AccountSnapshot(a)
		if err != nil {
			t.Fatal(err)
		}
		liveAccounts[a] = *st
	}
	_, liveBooks, err := e.Snapshot(accounts[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	// Sequential replay of the same stream must reconstruct everything.
	replayed, err := ReplayWithGenesis(path, genesis)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	for _, a := range accounts {
		g := replayed.Accounts[a]
		l := liveAccounts[a]
		if g.Cash != l.Cash || g.ReservedCash != l.ReservedCash || g.OpenOrders != l.OpenOrders {
			t.Fatalf("account %s differs replay=%+v live=%+v", a, g, l)
		}
		for sym := range l.Positions {
			if g.Positions[sym] != l.Positions[sym] || g.ReservedPositions[sym] != l.ReservedPositions[sym] {
				t.Fatalf("account %s symbol %s differs replay pos=%d/%d live pos=%d/%d",
					a, sym, g.Positions[sym], g.ReservedPositions[sym], l.Positions[sym], l.ReservedPositions[sym])
			}
		}
	}
	for sym, lb := range liveBooks {
		rb := replayed.Books[sym]
		if rb == nil {
			if len(lb.Bids)+len(lb.Asks) != 0 {
				t.Fatalf("book %s missing on replay", sym)
			}
			continue
		}
		if len(rb.Bids) != len(lb.Bids) || len(rb.Asks) != len(lb.Asks) {
			t.Fatalf("book %s depth differs replay=%+v live=%+v", sym, rb, lb)
		}
		for i := range lb.Bids {
			if rb.Bids[i] != lb.Bids[i] {
				t.Fatalf("book %s bid %d differs: %+v vs %+v", sym, i, rb.Bids[i], lb.Bids[i])
			}
		}
		for i := range lb.Asks {
			if rb.Asks[i] != lb.Asks[i] {
				t.Fatalf("book %s ask %d differs: %+v vs %+v", sym, i, rb.Asks[i], lb.Asks[i])
			}
		}
	}
	t.Logf("CONCURRENT run: %d orders created, %d trades journaled; replay matches live across %d accounts/%d symbols",
		idCounter, len(replayed.Trades), len(accounts), len(replayed.Books))

	// The replay itself is deterministic: run twice and compare.
	again, err := ReplayWithGenesis(path, genesis)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Events) != len(replayed.Events) || len(again.Trades) != len(replayed.Trades) {
		t.Fatal("repeated replay is not deterministic")
	}
}
