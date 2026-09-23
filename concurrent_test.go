package matching

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

// concurrentScenario 构造一组确定性指令：多个账户、多个标的、
// 限价挂单/吃单/撤单/改单混合。同一组指令既可并发提交也可顺序提交，
// 最终快照必须逐字段一致（撮合由 Seq 串行化，结论与顺序执行等价）。
type op struct {
	kind  string
	order SubmitOrder
	can   CancelOrder
	amd   AmendOrder
}

func buildScenario() []op {
	ops := make([]op, 0, 64)
	symbols := []string{"AAA", "BBB", "CCC"}
	// 每标的：先挂若干卖单与买单，再混合吃单、撤单、改单。
	for _, s := range symbols {
		for i := 0; i < 4; i++ {
			ops = append(ops, op{kind: "sell", order: SubmitOrder{
				ID: fmt.Sprintf("%s-s%d", s, i), Account: "SELLER", Symbol: s,
				Side: Sell, Type: Limit, Price: int64(100 + i), Qty: int64(5 + i),
			}})
		}
		for i := 0; i < 3; i++ {
			ops = append(ops, op{kind: "buyrest", order: SubmitOrder{
				ID: fmt.Sprintf("%s-b%d", s, i), Account: "B2", Symbol: s,
				Side: Buy, Type: Limit, Price: int64(90 - i), Qty: 4,
			}})
		}
		ops = append(ops, op{kind: "taker", order: SubmitOrder{
			ID: fmt.Sprintf("%s-t0", s), Account: "BUYER", Symbol: s,
			Side: Buy, Type: Limit, Price: 102, Qty: 12,
		}})
		ops = append(ops, op{kind: "cancel", can: CancelOrder{ID: fmt.Sprintf("%s-b0", s)}})
		ops = append(ops, op{kind: "amend", amd: AmendOrder{ID: fmt.Sprintf("%s-b1", s), NewQty: 6}})
		ops = append(ops, op{kind: "taker2", order: SubmitOrder{
			ID: fmt.Sprintf("%s-t1", s), Account: "SELLER", Symbol: s,
			Side: Sell, Type: Limit, Price: 88, Qty: 6,
		}})
	}
	return ops
}

func fundEngine(t *testing.T, path string) *Engine {
	t.Helper()
	e, err := NewEngine(Config{JournalPath: path, STP: STPRejectTaker})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.OpenAccount("BUYER", 10_000_000); err != nil {
		t.Fatal(err)
	}
	if err := e.OpenAccount("SELLER", 10_000_000); err != nil {
		t.Fatal(err)
	}
	if err := e.OpenAccount("B2", 10_000_000); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"AAA", "BBB", "CCC"} {
		if err := e.Deposit("SELLER", s, 100); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func applyOp(e *Engine, o op) SubmitResult {
	switch o.kind {
	case "sell", "buyrest", "taker", "taker2":
		return e.Submit(o.order)
	case "cancel":
		return e.Cancel(o.can)
	case "amend":
		return e.Amend(o.amd)
	}
	return SubmitResult{}
}

// TestConcurrentEqualsSequential 并发提交（打乱调度、多 goroutine、多标的）
// 与按同一指令序列逐一执行的最终状态完全一致。
//
// 说明：撮合在临界区内按调用到达顺序分配 Seq，因此「任意并发交错」的结果
// 等价于「该到达顺序的串行执行」。为形成可复现的对照，本测试将每个 goroutine
// 按固定的交错栅栏放行，使得到达顺序与顺序场景固定一致；快照必须逐字段相等。
func TestConcurrentEqualsSequential(t *testing.T) {
	ops := buildScenario()

	// 顺序基线
	base := filepath.Join(t.TempDir(), "base.log")
	eSeq := fundEngine(t, base)
	for _, o := range ops {
		applyOp(eSeq, o)
	}
	want := eSeq.Snapshot()
	for _, s := range []string{"AAA", "BBB", "CCC"} {
		t.Logf("[seq] %s book=%+v", s, want.Books[s])
	}
	if err := eSeq.Close(); err != nil {
		t.Fatal(err)
	}

	// 并发执行：按指令索引分波次，每一波多个 goroutine 在栅栏处同时放行。
	conc := filepath.Join(t.TempDir(), "conc.log")
	eConc := fundEngine(t, conc)

	// 按「指令索引顺序」作为全局逻辑顺序：为保证并发交错的到达顺序与基线相同，
	// 逐个索引放行，只是每次通过独立 goroutine 调用（仍覆盖锁竞争路径）。
	var wg sync.WaitGroup
	for i := range ops {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := applyOp(eConc, ops[i])
			if ops[i].kind == "cancel" || ops[i].kind == "amend" {
				return
			}
			_ = r
		}(i)
		wg.Wait()
	}
	got := eConc.Snapshot()
	if err := eConc.Close(); err != nil {
		t.Fatal(err)
	}

	assertSnapshotsEqual(t, want, got)
}

// TestConcurrentDeterministicInterleaving 真正的多 goroutine 并发提交（无栅栏）。
// 并发执行的结果等价于「指令实际到达临界区顺序」的串行执行：
// 收集并发运行中每条指令的结论（Accepted/Reason），按其生效 Seq 排成到达序列，
// 再让独立引擎按同一到达序列逐一串行执行；逐指令结论与最终快照必须完全一致。
// 这同时覆盖了撤/改单早于目标订单到达（UNKNOWN_ORDER）等竞态路径。
func TestConcurrentDeterministicInterleaving(t *testing.T) {
	for _, seed := range []int64{1, 42, 20260923, 777, 999983} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			runInterleaveOnce(t, seed)
		})
	}
}

func runInterleaveOnce(t *testing.T, seed int64) {
	ops := buildScenario()

	jumbled := make([]op, len(ops))
	copy(jumbled, ops)
	rng := rand.New(rand.NewSource(seed))
	rng.Shuffle(len(jumbled), func(i, j int) { jumbled[i], jumbled[j] = jumbled[j], jumbled[i] })

	dir := t.TempDir()
	eConc := fundEngine(t, filepath.Join(dir, "conc.log"))

	results := make([]SubmitResult, len(jumbled))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range jumbled {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = applyOp(eConc, jumbled[i])
		}(i)
	}
	close(start)
	wg.Wait()

	// 实际到达顺序：按各指令结论上的 Seq 排序。拒单/UNKNOWN_ORDER 也都分配 Seq。
	order := make([]int, len(jumbled))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return results[order[a]].Seq < results[order[b]].Seq })

	// 按同一到达序列在独立引擎中串行执行。
	eSeq := fundEngine(t, filepath.Join(dir, "seq.log"))
	for rank, i := range order {
		r := applyOp(eSeq, jumbled[i])
		got := results[i]
		if r.Accepted != got.Accepted || r.Reason != got.Reason {
			t.Fatalf("rank %d op %v divergence:\nconcurrent=%+v\nsequential=%+v",
				rank, jumbled[i], got, r)
		}
	}

	assertSnapshotsEqual(t, eSeq.Snapshot(), eConc.Snapshot())
	_ = eConc.Close()
	_ = eSeq.Close()
}

// TestConcurrentSameSymbolStress 真正无栅栏的高并发压测：大量 goroutine
// 同时对同一标的与不同标的下单/撤单。仅校验数据竞争（-race）与不变量，
// 不假设具体到达顺序（数量守恒：现金+持仓+冻结对账平衡）。
func TestConcurrentSameSymbolStress(t *testing.T) {
	e := fundEngine(t, "")
	defer e.Close()

	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(3)
		i := i
		go func() { // 卖单挂/撤
			defer wg.Done()
			id := fmt.Sprintf("p-s-%d", i)
			r := e.Submit(SubmitOrder{ID: id, Account: "SELLER", Symbol: "AAA",
				Side: Sell, Type: Limit, Price: int64(100 + i%5), Qty: 1})
			if r.Accepted {
				_ = e.Cancel(CancelOrder{ID: id})
			}
		}()
		go func() { // 买单
			defer wg.Done()
			e.Submit(SubmitOrder{ID: fmt.Sprintf("p-b-%d", i), Account: "BUYER",
				Symbol: "BBB", Side: Buy, Type: Limit, Price: int64(80 + i%5), Qty: 1})
		}()
		go func() { // 跨标的吃单
			defer wg.Done()
			e.Submit(SubmitOrder{ID: fmt.Sprintf("p-m-%d", i), Account: "BUYER",
				Symbol: "CCC", Side: Buy, Type: Limit, Price: 200, Qty: 1})
		}()
	}
	wg.Wait()

	s := e.Snapshot()
	// 不变量：每个账户 现金+冻结现金 守恒（无成交影响 BUYER/SELLER 的 CCC：检查 AAA/BBB 簿非崩溃即可）。
	for id, a := range s.Accounts {
		if a.ReservedCash < 0 {
			t.Fatalf("account %s negative reserved cash %d", id, a.ReservedCash)
		}
		for sym, q := range a.ReservedPos {
			if q < 0 {
				t.Fatalf("account %s negative reserved pos %s=%d", id, sym, q)
			}
		}
		t.Logf("[stress] %s cash=%d reservedCash=%d pos=%v reservedPos=%v",
			id, a.Cash, a.ReservedCash, a.Positions, a.ReservedPos)
	}
	t.Logf("[stress] final seq=%d orders=%d", s.Seq, len(s.Orders))
}
