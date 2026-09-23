package matching

import (
	"os"
	"path/filepath"
	"testing"
)

// buildJournaledScenario 在给定流水路径上执行一组含成交/撤改/拒单的指令。
func buildJournaledScenario(t *testing.T, path string) EngineSnapshot {
	t.Helper()
	e := fundEngine(t, path)
	ops := buildScenario()
	// 追加一些必被拒绝的指令，确认拒单同样进流水且回放可复现。
	rejects := []SubmitOrder{
		{ID: "bad-qty", Account: "BUYER", Symbol: "AAA", Side: Buy, Type: Limit, Price: 10, Qty: 0},
		{ID: "bad-cash", Account: "B2", Symbol: "AAA", Side: Buy, Type: Limit, Price: 1000, Qty: 100_000},
	}
	for _, o := range ops {
		applyOp(e, o)
	}
	for _, r := range rejects {
		rr := e.Submit(r)
		if rr.Accepted {
			t.Fatalf("expected reject for %s", r.ID)
		}
	}
	s := e.Snapshot()
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestReplayMatchesIncremental 任意时刻均可依据流水完整回放，
// 回放结论与增量运行一致；重复回放不产生副作用。
func TestReplayMatchesIncremental(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.log")
	want := buildJournaledScenario(t, path)

	// 第一次全新回放。
	e1, err := ReplayFromJournal(path, Config{STP: STPRejectTaker})
	if err != nil {
		t.Fatal(err)
	}
	got1 := e1.Snapshot()
	assertSnapshotsEqual(t, want, got1)
	if e1.journal != nil {
		t.Fatal("replay engine must not hold an append journal")
	}

	// 第二次重复回放：结论一致、文件字节不变（只读）。
	fiBefore, _ := os.Stat(path)
	e2, err := ReplayFromJournal(path, Config{STP: STPRejectTaker})
	if err != nil {
		t.Fatal(err)
	}
	assertSnapshotsEqual(t, want, e2.Snapshot())
	fiAfter, _ := os.Stat(path)
	if fiBefore.Size() != fiAfter.Size() {
		t.Fatalf("replay mutated journal: %d -> %d bytes", fiBefore.Size(), fiAfter.Size())
	}

	// 用 NewEngine 打开同一流水继续写入，再回放，增量与回放仍一致。
	e3, err := NewEngine(Config{JournalPath: path, STP: STPRejectTaker})
	if err != nil {
		t.Fatal(err)
	}
	e3.Submit(SubmitOrder{ID: "after-restart", Account: "BUYER", Symbol: "AAA",
		Side: Buy, Type: Limit, Price: 70, Qty: 2})
	want2 := e3.Snapshot()
	if err := e3.Close(); err != nil {
		t.Fatal(err)
	}
	e4, err := ReplayFromJournal(path, Config{STP: STPRejectTaker})
	if err != nil {
		t.Fatal(err)
	}
	assertSnapshotsEqual(t, want2, e4.Snapshot())
	t.Logf("replay idempotent and consistent through restart; final seq=%d", want2.Seq)
}

// TestCrashRecovery 模拟崩溃：在最后一条完整记录后追加半截行，
// 恢复时必须截断，且已提交记录全部保留。
func TestCrashRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.log")
	want := buildJournaledScenario(t, path)

	goodStat, _ := os.Stat(path)

	// 追加一条「写了一半」的记录（无换行、CRC/JSON 不完整）。
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	garbage := []byte("deadbeef|{\"seq\":9999,\"events\":[{\"type\":1}]}  ... torn write \x00\x01")
	if _, err := f.Write(garbage); err != nil {
		t.Fatal(err)
	}
	f.Sync()
	f.Close()

	tornStat, _ := os.Stat(path)
	if tornStat.Size() <= goodStat.Size() {
		t.Fatal("test setup failed: garbage not appended")
	}

	// 恢复：截断半截行。
	n, err := RecoverJournal(path)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != int64(len(garbage)) {
		t.Fatalf("truncated %d bytes, want %d", n, len(garbage))
	}
	recStat, _ := os.Stat(path)
	if recStat.Size() != goodStat.Size() {
		t.Fatalf("size after recover=%d want %d", recStat.Size(), goodStat.Size())
	}

	// 再恢复一次：无新截断（幂等）。
	n2, err := RecoverJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Fatalf("second recovery truncated %d, want 0", n2)
	}

	// 回放结论必须与崩溃前增量状态一致，未提交记录不产生任何副作用。
	e, err := ReplayFromJournal(path, Config{STP: STPRejectTaker})
	if err != nil {
		t.Fatal(err)
	}
	assertSnapshotsEqual(t, want, e.Snapshot())

	// 恢复后继续追加写读，流水仍然完整可用。
	e2, err := NewEngine(Config{JournalPath: path, STP: STPRejectTaker})
	if err != nil {
		t.Fatal(err)
	}
	r := e2.Submit(SubmitOrder{ID: "post-crash", Account: "BUYER", Symbol: "AAA",
		Side: Buy, Type: Limit, Price: 60, Qty: 1})
	if !r.Accepted {
		t.Fatalf("post-crash submit: %+v", r)
	}
	want2 := e2.Snapshot()
	e2.Close()
	e3, err := ReplayFromJournal(path, Config{STP: STPRejectTaker})
	if err != nil {
		t.Fatal(err)
	}
	assertSnapshotsEqual(t, want2, e3.Snapshot())
	t.Logf("crash recovery OK; restored seq=%d, post-crash seq=%d", want.Seq, want2.Seq)
}

// TestCorruptedRecordDetected 已提交记录的 CRC 被篡改时必须报错，绝不静默丢弃。
func TestCorruptedRecordDetected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.log")
	buildJournaledScenario(t, path)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 翻转第一条记录 payload 中的一个字节（跳过 CRC 前缀）。
	idx := indexByte(raw, '|')
	if idx < 0 {
		t.Fatal("no separator")
	}
	raw[idx+20] ^= 0xFF
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReplayFromJournal(path, Config{STP: STPRejectTaker}); err == nil {
		t.Fatal("corrupted record must be detected")
	} else {
		t.Logf("corruption correctly detected: %v", err)
	}
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
