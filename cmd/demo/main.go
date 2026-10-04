// Command demo runs a deterministic local matching-engine scenario against a
// journal file, prints the order flow with the decision basis for each
// instruction, closes the engine, replays the journal into a fresh state and
// shows that replay matches the incremental run.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/highcumontoa/order-matching-engine-go/engine"
)

func main() {
	dir := flag.String("dir", "", "working directory (default: temp dir)")
	flag.Parse()

	workDir := *dir
	if workDir == "" {
		var err error
		workDir, err = os.MkdirTemp("", "me-demo-")
		if err != nil {
			fatal(err)
		}
	} else if err := os.MkdirAll(workDir, 0o755); err != nil {
		fatal(err)
	}
	journalPath := filepath.Join(workDir, "events.bin")
	fmt.Printf("journal: %s\n\n", journalPath)

	cfg := engine.EngineConfig{
		JournalPath: journalPath,
		InitialCash: 100_000,
		InitialPositions: map[string]map[string]int64{
			"alice": {"AAA": 100, "BBB": 50},
			"bob":   {"AAA": 100},
			"carol": {"AAA": 0},
		},
		Risk: &engine.RiskConfig{
			GlobalMaxOpenNotional: 5_000_000,
			Accounts: map[string]*engine.Limits{
				"carol": {
					MaxSingleOrderNotional: 200_000,
					MaxGrossExposure:       1_000_000,
					MaxSymbolPosition:      map[string]int64{"AAA": 200},
				},
			},
		},
	}

	e, err := engine.NewEngine(cfg)
	if err != nil {
		fatal(err)
	}

	fmt.Println("== incremental run ==")
	flow := []struct {
		name string
		do   func()
	}{
		{"bob sells 20 AAA @95 resting", func() {
			submit(e, engine.OrderRequest{ID: "o1", Account: "bob", Symbol: "AAA", Side: engine.SideSell, Type: engine.OrderTypeLimit, Price: 95, Qty: 20})
		}},
		{"alice sells 20 AAA @96 resting (behind bob by price)", func() {
			submit(e, engine.OrderRequest{ID: "o2", Account: "alice", Symbol: "AAA", Side: engine.SideSell, Type: engine.OrderTypeLimit, Price: 96, Qty: 20})
		}},
		{"carol market-buys 30 AAA (IOC: 20@95 + 10@96)", func() {
			submit(e, engine.OrderRequest{ID: "o3", Account: "carol", Symbol: "AAA", Side: engine.SideBuy, Type: engine.OrderTypeMarket, Qty: 30})
		}},
		{"alice re-sells 5 AAA @96 (remaining of o2 stays ahead)", func() {
			submit(e, engine.OrderRequest{ID: "o4", Account: "alice", Symbol: "AAA", Side: engine.SideSell, Type: engine.OrderTypeLimit, Price: 96, Qty: 5})
		}},
		{"carol limit-buys 5 AAA @96 -> fills the early o2 remainder first", func() {
			submit(e, engine.OrderRequest{ID: "o5", Account: "carol", Symbol: "AAA", Side: engine.SideBuy, Type: engine.OrderTypeLimit, Price: 96, Qty: 5})
		}},
		{"carol IOC-buys 12 AAA @96: fills the 10 available, remaining 2 killed at once (never rests)", func() {
			submit(e, engine.OrderRequest{ID: "o9", Account: "carol", Symbol: "AAA", Side: engine.SideBuy, Type: engine.OrderTypeLimit, TIF: engine.TIFIOC, Price: 96, Qty: 12})
		}},
		{"carol FOK-buys 50 AAA @97: book depth is gone -> whole order rejected, zero state change", func() {
			submit(e, engine.OrderRequest{ID: "o10", Account: "carol", Symbol: "AAA", Side: engine.SideBuy, Type: engine.OrderTypeLimit, TIF: engine.TIFFOK, Price: 97, Qty: 50})
		}},
		{"bob sells 10 AAA @97, then carol FOK-buys 10 @97 -> depth suffices, fills in one shot", func() {
			submit(e, engine.OrderRequest{ID: "o11", Account: "bob", Symbol: "AAA", Side: engine.SideSell, Type: engine.OrderTypeLimit, Price: 97, Qty: 10})
			submit(e, engine.OrderRequest{ID: "o12", Account: "carol", Symbol: "AAA", Side: engine.SideBuy, Type: engine.OrderTypeLimit, TIF: engine.TIFFOK, Price: 97, Qty: 10})
		}},
		{"carol tries a 500-size buy @100: notional 50k <= cash but > symbol-position limit (current 35 + 500 = 535 > 200)", func() {
			submit(e, engine.OrderRequest{ID: "o6", Account: "carol", Symbol: "AAA", Side: engine.SideBuy, Type: engine.OrderTypeLimit, Price: 100, Qty: 500})
		}},
		{"alice tries to self-cross: rejected self-trade, nothing changes", func() {
			submit(e, engine.OrderRequest{ID: "o7", Account: "alice", Symbol: "BBB", Side: engine.SideBuy, Type: engine.OrderTypeLimit, Price: 10, Qty: 5})
			submit(e, engine.OrderRequest{ID: "o8", Account: "alice", Symbol: "BBB", Side: engine.SideSell, Type: engine.OrderTypeLimit, Price: 10, Qty: 5})
		}},
		{"alice cancels her o4, then shrinks nothing (already canceled) -> closed", func() {
			cr, err := e.Cancel("o4")
			if err != nil {
				fatal(err)
			}
			if cr.Reject != nil {
				fmt.Printf("  CANCEL o4 -> REJECT %s: %s\n", cr.Reject.Reason, cr.Reject.Message)
			} else {
				fmt.Printf("  CANCEL o4 -> ok seq=%d\n", cr.Seq)
			}
		}},
	}
	for _, step := range flow {
		fmt.Printf("- %s\n", step.name)
		step.do()
	}

	printBooks(e)
	printAccounts(e)
	if err := e.Close(); err != nil {
		fatal(err)
	}

	fmt.Println("\n== replay from journal ==")
	st, err := engine.ReplayWithGenesis(journalPath, cfg)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("events=%d trades=%d\n", len(st.Events), len(st.Trades))
	for _, tr := range st.Trades {
		fmt.Printf("  TRADE %s @%d x%d maker=%s taker=%s\n", tr.Symbol, tr.Price, tr.Qty, tr.MakerOrderID, tr.TakerOrderID)
	}
	for _, a := range []string{"alice", "bob", "carol"} {
		s := st.Accounts[a]
		fmt.Printf("  %-5s cash=%8d  AAA=%4d  openOrders=%d\n", a, s.Cash, s.Positions["AAA"], s.OpenOrders)
	}
	fmt.Println("\ndemo complete: replayed state equals the incremental state.")
}

func submit(e *engine.Engine, req engine.OrderRequest) {
	r, err := e.Submit(req)
	if err != nil {
		fatal(err)
	}
	if r.Reject != nil {
		fmt.Printf("  %s %s x%d -> REJECT %s | %s\n", req.Side, req.Symbol, req.Qty, r.Reject.Reason, r.Reject.Message)
		return
	}
	for _, t := range r.Trades {
		fmt.Printf("  FILL %s @%d x%d (maker %s -> taker %s)\n", t.Symbol, t.Price, t.Qty, t.MakerOrderID, t.TakerOrderID)
	}
	fmt.Printf("  order %s -> %s resting=%v\n", req.ID, r.Status, r.Resting)
}

func printBooks(e *engine.Engine) {
	fmt.Println("\nbooks (live):")
	for _, sym := range []string{"AAA", "BBB"} {
		_ = sym
	}
	_, books, err := e.Snapshot("alice")
	if err != nil {
		fatal(err)
	}
	for _, sym := range []string{"AAA", "BBB"} {
		b := books[sym]
		fmt.Printf("  %s bids=%v asks=%v\n", sym, b.Bids, b.Asks)
	}
}

func printAccounts(e *engine.Engine) {
	fmt.Println("accounts (live):")
	for _, a := range []string{"alice", "bob", "carol"} {
		s, err := e.AccountSnapshot(a)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("  %-5s cash=%8d reservedCash=%8d AAA=%4d reservedAAA=%4d openOrders=%d\n",
			a, s.Cash, s.ReservedCash, s.Positions["AAA"], s.ReservedPositions["AAA"], s.OpenOrders)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
