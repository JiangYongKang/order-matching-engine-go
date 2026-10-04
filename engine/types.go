// Package engine implements a local in-memory order matching and execution
// engine with deterministic replay support.
package engine

import "time"

// Side identifies whether an order buys or sells.
type Side int

const (
	SideBuy Side = iota + 1
	SideSell
)

func (s Side) String() string {
	switch s {
	case SideBuy:
		return "BUY"
	case SideSell:
		return "SELL"
	default:
		return "UNKNOWN"
	}
}

// OrderType distinguishes limit and market orders.
type OrderType int

const (
	OrderTypeLimit OrderType = iota + 1
	OrderTypeMarket
)

func (t OrderType) String() string {
	switch t {
	case OrderTypeLimit:
		return "LIMIT"
	case OrderTypeMarket:
		return "MARKET"
	default:
		return "UNKNOWN"
	}
}

// TimeInForce controls how long an order may stay live after submission.
type TimeInForce int

const (
	// TIFGTC (good-til-canceled) is the default: a limit remainder rests in
	// the book until filled or canceled. Market orders always behave as IOC.
	TIFGTC TimeInForce = iota
	// TIFIOC (immediate-or-cancel): fill whatever is matchable right now and
	// cancel the remainder immediately; the order never rests and never
	// reserves funds/shares beyond what actually fills.
	TIFIOC
	// TIFFOK (fill-or-kill): if the full quantity cannot be matched right
	// now, the whole order is rejected with zero state change; otherwise it
	// fills entirely in one shot and never rests.
	TIFFOK
)

func (t TimeInForce) String() string {
	switch t {
	case TIFGTC:
		return "GTC"
	case TIFIOC:
		return "IOC"
	case TIFFOK:
		return "FOK"
	default:
		return "UNKNOWN"
	}
}

// OrderStatus is the lifecycle state of an order.
type OrderStatus int

const (
	StatusNew OrderStatus = iota + 1
	StatusPartiallyFilled
	StatusFilled
	StatusCanceled
	StatusRejected
)

func (s OrderStatus) String() string {
	switch s {
	case StatusNew:
		return "NEW"
	case StatusPartiallyFilled:
		return "PARTIALLY_FILLED"
	case StatusFilled:
		return "FILLED"
	case StatusCanceled:
		return "CANCELED"
	case StatusRejected:
		return "REJECTED"
	default:
		return "UNKNOWN"
	}
}

// Order is an order as tracked by the engine.
type Order struct {
	ID          string
	Account     string
	Symbol      string
	Side        Side
	Type        OrderType
	TIF         TimeInForce
	Price       int64 // limit price; 0 for market orders
	Qty         int64 // original quantity
	OpenQty     int64 // unfilled quantity
	FilledQty   int64
	PrioritySeq int64 // global monotonic sequence defining time priority
	CreatedAt   time.Time
	// TerminalCancel marks that the order ended by cancellation (explicit
	// cancel, or IOC/market remainder kill) rather than by filling out, so a
	// partially filled canceled order is distinguishable from a filled one.
	TerminalCancel bool
}

// Status derives the status of the order.
func (o *Order) Status() OrderStatus {
	switch {
	case o.OpenQty == 0 && o.TerminalCancel:
		return StatusCanceled
	case o.OpenQty == 0 && o.FilledQty > 0:
		return StatusFilled
	case o.OpenQty == 0:
		return StatusCanceled
	case o.FilledQty > 0:
		return StatusPartiallyFilled
	default:
		return StatusNew
	}
}

// Active reports whether the order is still resting in the book.
func (o *Order) Active() bool {
	return o.OpenQty > 0
}

// OrderRequest is a submitted order instruction.
type OrderRequest struct {
	ID      string
	Account string
	Symbol  string
	Side    Side
	Type    OrderType
	TIF     TimeInForce // zero value = TIFGTC (rest); market orders are IOC regardless
	Price   int64       // required (positive) for limit orders; ignored for market
	Qty     int64
}

// Trade records one fill between a taker (incoming) order and a resting maker.
type Trade struct {
	Seq          int64
	Symbol       string
	Price        int64
	Qty          int64
	MakerOrderID string
	MakerAccount string
	TakerOrderID string
	TakerAccount string
	Time         time.Time
}

// SubmitResult is the outcome of a submit instruction.
type SubmitResult struct {
	Order   *Order
	Trades  []*Trade
	Status  OrderStatus
	Reject  *RejectError
	Seq     int64
	Resting bool
}

// CancelResult is the outcome of a cancel instruction.
type CancelResult struct {
	OrderID string
	Seq     int64
	Reject  *RejectError
}

// ReplaceResult is the outcome of a replace (amend) instruction.
type ReplaceResult struct {
	Order        *Order
	Trades       []*Trade
	Reject       *RejectError
	Seq          int64
	KeptPriority bool
}

// EventType enumerates the entries of the deterministic event journal.
type EventType int

const (
	EventAccepted EventType = iota + 1
	EventTrade
	EventCanceled
	EventReplaced
	EventRejected
)

func (t EventType) String() string {
	switch t {
	case EventAccepted:
		return "ACCEPTED"
	case EventTrade:
		return "TRADE"
	case EventCanceled:
		return "CANCELED"
	case EventReplaced:
		return "REPLACED"
	case EventRejected:
		return "REJECTED"
	default:
		return "UNKNOWN"
	}
}

// Event is one persisted journal entry. Replay applies entries in Seq order.
type Event struct {
	Seq  int64
	Type EventType
	Time time.Time

	// Accepted / Rejected / Canceled / Replaced identify an order.
	OrderID string

	// Accepted / Rejected fields.
	Order   *Order // accepted resting order (nil for market orders / rejected)
	Account string
	Symbol  string
	Side    Side
	OType   OrderType
	TIF     TimeInForce
	Price   int64
	Qty     int64
	Reason  string // rejection reason (Rejected)

	// Amount of account funds/positions frozen for an accepted order.
	ReservedCash   int64
	ReservedShares int64

	// Trade fields.
	Trade *Trade

	// Replaced fields.
	KeptPriority bool
}

// AccountState is the public snapshot of an account.
type AccountState struct {
	ID                string
	Cash              int64
	ReservedCash      int64
	Positions         map[string]int64
	ReservedPositions map[string]int64 // shares frozen by resting sell orders
	OpenOrders        int
}

// BookLevel is one price level of an order book snapshot.
type BookLevel struct {
	Price int64
	Qty   int64
}

// BookSnapshot is an order book state at a point in time.
type BookSnapshot struct {
	Symbol string
	Bids   []BookLevel
	Asks   []BookLevel
}

// Limits defines per-account risk constraints.
type Limits struct {
	MaxSingleOrderNotional int64            // cap on price*qty of one order
	MaxGrossExposure       int64            // cap on sum of all resting order notional
	MaxSymbolPosition      map[string]int64 // cap on |position + open net qty| per symbol
}

// RiskConfig holds global and per-account risk configuration.
type RiskConfig struct {
	GlobalMaxOpenNotional int64
	Accounts              map[string]*Limits
}

// LimitsFor returns the limits applicable to an account (nil if none).
func (r *RiskConfig) LimitsFor(account string) *Limits {
	if r == nil {
		return nil
	}
	return r.Accounts[account]
}

// EngineConfig configures an Engine.
type EngineConfig struct {
	// JournalPath is the file used for the durable event stream.
	JournalPath string
	// InitialCash / InitialPositions seed accounts at creation time.
	InitialCash      int64
	InitialPositions map[string]map[string]int64
	// InitialAccounts are additional accounts (no initial positions). Every
	// seeded account (here or via InitialPositions) receives InitialCash.
	InitialAccounts []string
	// Risk is the risk constraint configuration.
	Risk *RiskConfig
	// Clock, when set, supplies event times (tests use this for determinism).
	Clock func() time.Time
}

// ReplayState is the reconstructed result of reading an event stream.
type ReplayState struct {
	Accounts map[string]*AccountState
	Books    map[string]*BookSnapshot
	Trades   []*Trade
	Events   []*Event
}
