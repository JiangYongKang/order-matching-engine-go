package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func journalFixture(t *testing.T) (*Engine, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.bin")
	positions := map[string]map[string]int64{
		"alice": {"X": 100},
		"bob":   {"X": 100},
	}
	e, err := NewEngine(EngineConfig{
		JournalPath:      path,
		InitialCash:      1_000_000,
		InitialPositions: positions,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e, path
}

func accountRow(t *testing.T, st *ReplayState, a string) *AccountState {
	t.Helper()
	x := st.Accounts[a]
	if x == nil {
		t.Fatalf("account %s absent in replay state", a)
	}
	return x
}

func TestReplayEqualsIncremental(t *testing.T) {
	e, path := journalFixture(t)
	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "bob", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 5})
	mustSubmit(t, e, OrderRequest{ID: "b1", Account: "alice", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 101, Qty: 3})
	if _, err := e.Cancel("s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Replace("b1", 101, 3); err != nil { // same-price same-qty amend
		t.Fatal(err)
	}
	liveAlice, _ := e.AccountSnapshot("alice")
	liveBob, _ := e.AccountSnapshot("bob")
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	genesis := EngineConfig{InitialCash: 1_000_000,
		InitialPositions: map[string]map[string]int64{"alice": {"X": 100}, "bob": {"X": 100}}}
	st, err := ReplayWithGenesis(path, genesis)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	t.Logf("REPLAY %d events, %d trades", len(st.Events), len(st.Trades))
	for _, ev := range st.Events {
		switch ev.Type {
		case EventTrade:
			t.Logf("  seq=%d TRADE %s@%d x%d %s->%s", ev.Seq, ev.Trade.Symbol, ev.Trade.Price, ev.Trade.Qty, ev.Trade.MakerOrderID, ev.Trade.TakerOrderID)
		default:
			t.Logf("  seq=%d %s order=%s", ev.Seq, ev.Type, ev.OrderID)
		}
	}
	ra := accountRow(t, st, "alice")
	rb := accountRow(t, st, "bob")
	if ra.Cash != liveAlice.Cash || ra.Positions["X"] != liveAlice.Positions["X"] || ra.ReservedCash != liveAlice.ReservedCash {
		t.Fatalf("alice replay %+v != live %+v", ra, liveAlice)
	}
	if rb.Cash != liveBob.Cash || rb.Positions["X"] != liveBob.Positions["X"] || rb.ReservedPositions["X"] != liveBob.ReservedPositions["X"] {
		t.Fatalf("bob replay %+v != live %+v", rb, liveBob)
	}
	if st.Books["X"] == nil || len(st.Books["X"].Asks) != 0 {
		t.Fatalf("book replay wrong: %+v", st.Books["X"])
	}

	// Repeated replay is side-effect free and identical.
	st2, err := ReplayWithGenesis(path, genesis)
	if err != nil {
		t.Fatal(err)
	}
	if len(st2.Events) != len(st.Events) || len(st2.Trades) != len(st.Trades) {
		t.Fatalf("second replay differs: %d events vs %d", len(st2.Events), len(st.Events))
	}
}

func TestRecoveryContinuesAfterRestart(t *testing.T) {
	e, path := journalFixture(t)
	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "bob", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 5})
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart: recovered book still shows the resting sell.
	e2, err := NewEngine(EngineConfig{JournalPath: path, InitialCash: 1_000_000,
		InitialPositions: map[string]map[string]int64{"alice": {"X": 100}, "bob": {"X": 100}}})
	if err != nil {
		t.Fatal(err)
	}
	bookBefore, booksBefore, _ := e2.Snapshot("bob")
	_ = bookBefore
	if lv := booksBefore["X"]; len(lv.Asks) != 1 || lv.Asks[0].Qty != 5 {
		t.Fatalf("recovered book must retain resting sell, got %+v", lv.Asks)
	}
	r := mustSubmit(t, e2, OrderRequest{ID: "b1", Account: "alice", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Qty: 5})
	if len(r.Trades) != 1 || r.Trades[0].MakerOrderID != "s1" {
		t.Fatalf("post-restart must match recovered sell, got %+v", r.Trades)
	}
	if err := e2.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart again and replay: trades from both sessions are present once.
	st, err := Replay(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Trades) != 1 {
		t.Fatalf("exactly one trade must survive double recovery, got %d", len(st.Trades))
	}
}

func TestTornTailTruncation(t *testing.T) {
	e, path := journalFixture(t)
	mustSubmit(t, e, OrderRequest{ID: "s1", Account: "bob", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 5})
	mustSubmit(t, e, OrderRequest{ID: "b1", Account: "alice", Symbol: "X", Side: SideBuy, Type: OrderTypeLimit, Price: 101, Qty: 5})
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	fi, _ := os.Stat(path)
	size := fi.Size()
	// Simulate a crash mid-write: append a partial frame (valid header start,
	// truncated payload + garbage), then recover.
	garbage := []byte{0x31, 0x44, 0x45, 0x4D, 0xFF, 0x00, 0x00, 0x00, 0x00, 0x00}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(garbage); err != nil {
		t.Fatal(err)
	}
	f.Close()

	e2, err := NewEngine(EngineConfig{JournalPath: path, InitialCash: 1_000_000,
		InitialPositions: map[string]map[string]int64{"alice": {"X": 100}, "bob": {"X": 100}}})
	if err != nil {
		t.Fatalf("recover with torn tail: %v", err)
	}
	st, _, _ := e2.Snapshot("alice")
	if st.Positions["X"] != 105 {
		t.Fatalf("recovered position should reflect pre-crash trades, got %d", st.Positions["X"])
	}
	// New events continue sequencing after the recovered last sequence.
	mustSubmit(t, e2, OrderRequest{ID: "s2", Account: "bob", Symbol: "X", Side: SideSell, Type: OrderTypeLimit, Price: 100, Qty: 1})
	if err := e2.Close(); err != nil {
		t.Fatal(err)
	}
	fi2, _ := os.Stat(path)
	if fi2.Size() < size {
		t.Fatalf("journal should not shrink below last good byte after recovery append, size=%d", fi2.Size())
	}
}

// TestTruncatedPayloadKeepsGoodFrames writes a fully valid frame then a frame
// whose payload is cut halfway: open must keep the good one and drop only the
// partial tail.
func TestTruncatedPayloadKeepsGoodFrames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tail.bin")
	j, _, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	good := &Event{Seq: 1, Type: EventRejected, OrderID: "g1", Reason: "X"}
	payload, err := encodeEvent(good)
	if err != nil {
		t.Fatal(err)
	}
	writeFrame := func(ev *Event) []byte {
		pl, _ := encodeEvent(ev)
		hdr := make([]byte, frameHeader)
		binaryPutMagic(hdr)
		putLE32(hdr[4:8], uint32(len(pl)))
		putLE32(hdr[8:12], crc32Checksum(pl))
		return append(hdr, pl...)
	}
	_ = payload
	if err := j.Append(good); err != nil {
		t.Fatal(err)
	}
	// Build a correct frame for a second event then chop its payload.
	bad := &Event{Seq: 2, Type: EventCanceled, OrderID: "g2"}
	full := writeFrame(bad)
	partial := append([]byte{}, full[:len(full)-7]...)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(partial); err != nil {
		t.Fatal(err)
	}
	f.Close()
	j.Close()

	j2, events, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].OrderID != "g1" {
		t.Fatalf("only the good frame must survive, got %+v", events)
	}
	t.Logf("TORN partial frame dropped, retained seq=%d id=%s", events[0].Seq, events[0].OrderID)
	// Appending after recovery must continue at seq 1.
	e2 := &Event{Seq: 2, Type: EventRejected, OrderID: "g2b", Reason: "Y"}
	if err := j2.Append(e2); err != nil {
		t.Fatal(err)
	}
	j2.Close()
	_, events2, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events2) != 2 {
		t.Fatalf("expected 2 valid frames after recovery append, got %d", len(events2))
	}
}
