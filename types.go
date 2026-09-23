package matching

// Side 订单方向。
type Side int

const (
	Buy Side = iota + 1
	Sell
)

func (s Side) String() string {
	switch s {
	case Buy:
		return "BUY"
	case Sell:
		return "SELL"
	default:
		return "UNKNOWN"
	}
}

func (s Side) opposite() Side {
	if s == Buy {
		return Sell
	}
	return Buy
}

// OrderType 订单类型。
type OrderType int

const (
	Limit OrderType = iota + 1
	Market
)

func (t OrderType) String() string {
	switch t {
	case Limit:
		return "LIMIT"
	case Market:
		return "MARKET"
	default:
		return "UNKNOWN"
	}
}

// OrderStatus 订单生命周期状态。
type OrderStatus int

const (
	StatusNew OrderStatus = iota + 1
	StatusPartiallyFilled
	StatusFilled
	StatusCancelled
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
	case StatusCancelled:
		return "CANCELLED"
	case StatusRejected:
		return "REJECTED"
	default:
		return "UNKNOWN"
	}
}

// RejectReason 拒单原因，彼此可区分。
type RejectReason int

const (
	RejNone RejectReason = iota
	RejInvalidQuantity
	RejInvalidPrice
	RejUnknownAccount
	RejDuplicateOrderID
	RejUnknownOrder
	RejOrderClosed
	RejInsufficientFunds
	RejInsufficientPosition
	RejRiskAccountLimit
	RejRiskSymbolLimit
	RejRiskGlobalLimit
	RejSelfTrade
	RejAmendPriceChange
	RejAmendInvalidQty
	RejNoLiquidity
	RejInvalidParam
	RejJournalIO
)

func (r RejectReason) String() string {
	switch r {
	case RejNone:
		return "NONE"
	case RejInvalidQuantity:
		return "INVALID_QUANTITY"
	case RejInvalidPrice:
		return "INVALID_PRICE"
	case RejUnknownAccount:
		return "UNKNOWN_ACCOUNT"
	case RejDuplicateOrderID:
		return "DUPLICATE_ORDER_ID"
	case RejUnknownOrder:
		return "UNKNOWN_ORDER"
	case RejOrderClosed:
		return "ORDER_CLOSED"
	case RejInsufficientFunds:
		return "INSUFFICIENT_FUNDS"
	case RejInsufficientPosition:
		return "INSUFFICIENT_POSITION"
	case RejRiskAccountLimit:
		return "RISK_ACCOUNT_LIMIT"
	case RejRiskSymbolLimit:
		return "RISK_SYMBOL_LIMIT"
	case RejRiskGlobalLimit:
		return "RISK_GLOBAL_LIMIT"
	case RejSelfTrade:
		return "SELF_TRADE"
	case RejAmendPriceChange:
		return "AMEND_PRICE_CHANGE_FORBIDDEN"
	case RejAmendInvalidQty:
		return "AMEND_INVALID_QUANTITY"
	case RejNoLiquidity:
		return "NO_LIQUIDITY"
	case RejInvalidParam:
		return "INVALID_PARAM"
	case RejJournalIO:
		return "JOURNAL_IO_ERROR"
	default:
		return "UNKNOWN"
	}
}

// SelfTradePolicy 自成交处置策略。
type SelfTradePolicy int

const (
	// STPCancelMaker 撤销被动方（挂单），主动单继续向下撮合。
	STPCancelMaker SelfTradePolicy = iota + 1
	// STPRejectTaker 拒绝主动单，订单簿与账户状态保持不变。
	STPRejectTaker
)

// Order 订单运行时实体。金额单位为最小货币单位（整数），数量为整数。
type Order struct {
	ID        string
	Account   string
	Symbol    string
	Side      Side
	Type      OrderType
	Price     int64 // 限价单价格；市价单为 0
	OrigQty   int64
	FilledQty int64
	Status    OrderStatus
	Seq       int64 // 全引擎单调递增的时间优先令牌
	CreatedAt int64 // 提交时墙钟时间（毫秒，仅用于日志）
}

func (o *Order) Remaining() int64 {
	return o.OrigQty - o.FilledQty
}

// SubmitOrder 下单指令。
type SubmitOrder struct {
	ID      string
	Account string
	Symbol  string
	Side    Side
	Type    OrderType
	Price   int64
	Qty     int64
}

// CancelOrder 撤单指令。
type CancelOrder struct {
	ID string
}

// AmendOrder 改单指令：仅允许同价改量，保留队列优先级。
type AmendOrder struct {
	ID       string
	NewPrice int64
	NewQty   int64
}

// FillView 一次成交的视图，用于结果返回与日志。
type FillView struct {
	Seq       int64  `json:"seq"`
	Time      int64  `json:"time"`
	Symbol    string `json:"symbol"`
	TakerID   string `json:"taker_id"`
	MakerID   string `json:"maker_id"`
	TakerSide Side   `json:"taker_side"`
	Price     int64  `json:"price"`
	Qty       int64  `json:"qty"`
}

// SubmitResult 指令处理结论。
type SubmitResult struct {
	OrderID  string
	Seq      int64
	Status   OrderStatus
	Reason   RejectReason
	Detail   string
	Fills    []FillView
	Accepted bool
	// Err 非 nil 表示流水持久化失败：指令未改变内存状态，可在恢复后重试。
	Err error
}
