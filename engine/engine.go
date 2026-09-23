package engine

import (
	"fmt"
	"sync"
	"time"
)

// Engine is the local matching and execution engine.
//
// Concurrency model:
//   - each symbol has a single-writer shard goroutine, so order books are
//     never touched concurrently;
//   - a single global lock guards cross-symbol state (accounts, reservations,
//     risk counters, registries) and journal appends. Every committed
//     instruction appends its event group while holding it, which defines one
//     total event order. Replaying that order reconstructs the exact state,
//     hence concurrent submissions are equivalent to sequential execution in
//     journal order and are free of data races.
type Engine struct {
	cfg    EngineConfig
	jr     *Journal
	risk   *RiskChecker
	mu     sync.Mutex
	view   *replayView
	closed bool
	seqErr error // sticky journal failure; engine refuses further mutations

	shardMu   sync.Mutex
	shards    map[string]*shard
	closeOnce sync.Once
	wg        sync.WaitGroup

	clockFn func() time.Time
	nextSeq int64
}

// NewEngine creates an engine from config and recovers durable state from an
// existing journal (creating the file if absent). An empty journal path
// yields a fully in-memory engine (events are not persisted).
func NewEngine(cfg EngineConfig) (*Engine, error) {
	e := &Engine{
		cfg:    cfg,
		shards: make(map[string]*shard),
	}
	e.risk = NewRiskChecker(cfg.Risk)
	if cfg.Clock != nil {
		// clock stored indirectly via method override below
		e.clockFn = cfg.Clock
	}

	v := newReplayView()
	// Seed accounts from configuration. InitialPositions names also get cash.
	for a, pos := range cfg.InitialPositions {
		acct := v.ledger.ensureAccount(a)
		acct.cash = cfg.InitialCash
		for sym, qty := range pos {
			acct.positions[sym] = qty
		}
	}
	for _, a := range cfg.InitialAccounts {
		if !v.ledger.HasAccount(a) {
			v.ledger.ensureAccount(a).cash = cfg.InitialCash
		}
	}

	if cfg.JournalPath != "" {
		jr, events, err := OpenJournal(cfg.JournalPath)
		if err != nil {
			return nil, err
		}
		e.jr = jr
		// Configuration balances are the genesis ledger; the journal is
		// applied on top of them exactly once.
		rv, err := applyAllInto(events, v)
		if err != nil {
			return nil, fmt.Errorf("recover: %w", err)
		}
		v = rv
		e.nextSeq = int64(len(events))
	}
	e.view = v
	return e, nil
}

// clockFn may be overridden by configuration for deterministic timestamps.
var defaultClock = func() time.Time { return time.Now().UTC() }

// Submit places a limit or market order.
func (e *Engine) Submit(req OrderRequest) (*SubmitResult, error) {
	if err := e.errIfDone(); err != nil {
		return nil, err
	}
	s := e.shardFor(req.Symbol)
	ch := make(chan cmdResult, 1)
	s.ch <- command{kind: cmdSubmit, submit: req, respCh: ch}
	r := <-ch
	return r.submit, r.err
}

// Cancel withdraws an active resting order. Routing uses the symbol recorded
// for the order, so only its owning shard touches its book.
func (e *Engine) Cancel(orderID string) (*CancelResult, error) {
	if err := e.errIfDone(); err != nil {
		return nil, err
	}
	sym, ok := e.symbolOfOrder(orderID)
	if !ok {
		return &CancelResult{OrderID: orderID, Reject: rejectf(RejectOrderNotFound,
			"order %q does not exist", orderID)}, nil
	}
	s := e.shardFor(sym)
	ch := make(chan cmdResult, 1)
	s.ch <- command{kind: cmdCancel, orderID: orderID, respCh: ch}
	r := <-ch
	return r.cancel, r.err
}

// Replace amends price and/or quantity of a resting limit order.
func (e *Engine) Replace(orderID string, newPrice, newQty int64) (*ReplaceResult, error) {
	if err := e.errIfDone(); err != nil {
		return nil, err
	}
	sym, ok := e.symbolOfOrder(orderID)
	if !ok {
		return &ReplaceResult{Reject: rejectf(RejectOrderNotFound,
			"order %q does not exist", orderID)}, nil
	}
	s := e.shardFor(sym)
	ch := make(chan cmdResult, 1)
	s.ch <- command{kind: cmdReplace, orderID: orderID, newPrice: newPrice, newQty: newQty, respCh: ch}
	r := <-ch
	return r.replace, r.err
}

// AccountSnapshot returns a consistent snapshot of one account.
func (e *Engine) AccountSnapshot(account string) (*AccountState, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.view.ledger.HasAccount(account) {
		return nil, fmt.Errorf("account %q not found", account)
	}
	st := e.view.ledger.Snapshot(account)
	return &st, nil
}

// Snapshot returns one account plus all books at a consistent point. Callers
// that need strict cross-shard consistency should ensure no instruction is in
// flight (all submitted commands have returned).
func (e *Engine) Snapshot(account string) (*AccountState, map[string]BookSnapshot, error) {
	books := make(map[string]BookSnapshot)
	e.mu.Lock()
	for sym, b := range e.view.books {
		books[sym] = b.Snapshot()
	}
	var st *AccountState
	if e.view.ledger.HasAccount(account) {
		s := e.view.ledger.Snapshot(account)
		st = &s
	}
	e.mu.Unlock()
	if st == nil {
		return nil, books, fmt.Errorf("account %q not found", account)
	}
	return st, books, nil
}

// Order returns the current view of an order (filled/resting), if known.
func (e *Engine) Order(id string) (*Order, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	o, ok := e.view.orders[id]
	return o, ok
}

// Close stops all shards, waits for in-flight commands and closes the journal.
func (e *Engine) Close() error {
	var err error
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed = true
		e.mu.Unlock()
		e.shardMu.Lock()
		for _, s := range e.shards {
			close(s.ch)
		}
		shardList := make([]*shard, 0, len(e.shards))
		for _, s := range e.shards {
			shardList = append(shardList, s)
		}
		e.shardMu.Unlock()
		e.wg.Wait()
		_ = shardList
		if e.jr != nil {
			err = e.jr.Close()
		}
	})
	return err
}

// Replay reconstructs state purely from a journal file. It never writes, never
// mutates global state and can be run repeatedly with identical results.
func Replay(path string) (*ReplayState, error) {
	return ReplayWithGenesis(path, EngineConfig{})
}

// ReplayWithGenesis replays a journal on top of genesis cash/positions given
// by config (same semantics as engine recovery on restart).
func ReplayWithGenesis(path string, cfg EngineConfig) (*ReplayState, error) {
	events, err := ReadJournal(path)
	if err != nil {
		return nil, err
	}
	genesis := newReplayView()
	for a, pos := range cfg.InitialPositions {
		acct := genesis.ledger.ensureAccount(a)
		acct.cash = cfg.InitialCash
		for sym, q := range pos {
			acct.positions[sym] = q
		}
	}
	for _, a := range cfg.InitialAccounts {
		if !genesis.ledger.HasAccount(a) {
			genesis.ledger.ensureAccount(a).cash = cfg.InitialCash
		}
	}
	v, err := applyAllInto(events, genesis)
	if err != nil {
		return nil, err
	}
	return v.state(), nil
}

// ---- internals ----

type cmdKind int

const (
	cmdSubmit cmdKind = iota + 1
	cmdCancel
	cmdReplace
)

type command struct {
	kind     cmdKind
	submit   OrderRequest
	orderID  string
	newPrice int64
	newQty   int64
	respCh   chan cmdResult
}

type cmdResult struct {
	submit  *SubmitResult
	cancel  *CancelResult
	replace *ReplaceResult
	err     error
}

type shard struct {
	symbol string
	book   *Book
	ch     chan command
}

func (e *Engine) shardFor(symbol string) *shard {
	e.shardMu.Lock()
	defer e.shardMu.Unlock()
	s := e.shards[symbol]
	if s != nil {
		return s
	}
	// Bind the shard to the recovered book when present so the shard mutates
	// exactly the structure replay produced.
	b := e.view.books[symbol]
	if b == nil {
		b = NewBook(symbol)
		e.mu.Lock()
		e.view.books[symbol] = b
		e.mu.Unlock()
	}
	s = &shard{symbol: symbol, book: b, ch: make(chan command, 256)}
	e.shards[symbol] = s
	e.wg.Add(1)
	go e.runShard(s)
	return s
}

func (e *Engine) runShard(s *shard) {
	defer e.wg.Done()
	for cmd := range s.ch {
		cmd.respCh <- e.handle(s, cmd)
	}
}

func (e *Engine) handle(s *shard, c command) cmdResult {
	switch c.kind {
	case cmdSubmit:
		p := e.buildSubmitPlan(s, c.submit)
		res, err := e.commitSubmit(p)
		return cmdResult{submit: res, err: err}
	case cmdCancel:
		res, err := e.commitCancel(c.orderID)
		return cmdResult{cancel: res, err: err}
	case cmdReplace:
		res, err := e.commitReplace(s, c.orderID, c.newPrice, c.newQty)
		return cmdResult{replace: res, err: err}
	default:
		return cmdResult{err: fmt.Errorf("unknown command kind %d", c.kind)}
	}
}

func (e *Engine) symbolOfOrder(id string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if o := e.view.orders[id]; o != nil {
		return o.Symbol, true
	}
	return "", false
}

func (e *Engine) errIfDone() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return fmt.Errorf("engine is closed")
	}
	if e.seqErr != nil {
		return fmt.Errorf("engine journal failed: %w", e.seqErr)
	}
	return nil
}

func (e *Engine) clock() time.Time {
	if e.clockFn != nil {
		return e.clockFn()
	}
	return defaultClock()
}

// allocSeqLocked hands out the next global event sequence. It must be called
// with the global lock held.
func (e *Engine) allocSeqLocked() int64 {
	e.nextSeq++
	return e.nextSeq
}

// seekSeqLocked advances the counter when several events of one instruction
// are numbered directly (without a per-event alloc call), keeping the stream
// contiguous.
func (e *Engine) seekSeqLocked(seq int64) {
	if seq > e.nextSeq {
		e.nextSeq = seq
	}
}

// persistAndApplyLocked appends each event durably, then applies it to the
// live (replay-derived) view using the same state machine as standalone
// replay. Journaling happens first: on crash after sync, recovery reapplies
// the event; on crash before, the torn tail is discarded. A failure aborts
// the whole instruction without applying anything else.
func (e *Engine) persistAndApplyLocked(events []*Event) error {
	for _, ev := range events {
		if e.jr != nil {
			if err := e.jr.Append(ev); err != nil {
				e.seqErr = err
				return err
			}
		}
		if err := e.view.applyEvent(ev); err != nil {
			return fmt.Errorf("apply event %d: %w", ev.Seq, err)
		}
	}
	return nil
}
