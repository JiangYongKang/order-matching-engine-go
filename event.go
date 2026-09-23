package matching

// EventType 事件类型。
type EventType int

const (
	EvAccountOpened EventType = iota + 1
	EvOrderAccepted
	EvOrderRejected
	EvOrderFilled
	EvOrderCancelled
	EvOrderAmended
	EvDeposit
)

func (t EventType) String() string {
	switch t {
	case EvAccountOpened:
		return "ACCOUNT_OPENED"
	case EvOrderAccepted:
		return "ORDER_ACCEPTED"
	case EvOrderRejected:
		return "ORDER_REJECTED"
	case EvOrderFilled:
		return "ORDER_FILLED"
	case EvOrderCancelled:
		return "ORDER_CANCELLED"
	case EvOrderAmended:
		return "ORDER_AMENDED"
	case EvDeposit:
		return "DEPOSIT"
	default:
		return "UNKNOWN"
	}
}

// Event 原子状态迁移事件，采用扁平结构以便 JSON 持久化与回放。
// 不同 EventType 使用不同字段集合。
type Event struct {
	Type   EventType `json:"type"`
	Seq    int64     `json:"seq"`
	Time   int64     `json:"time"`
	Symbol string    `json:"symbol,omitempty"`

	// ACCOUNT_OPENED
	Account string `json:"account,omitempty"`
	Cash    int64  `json:"cash,omitempty"`

	// ORDER_ACCEPTED / ORDER_REJECTED
	Order *Order `json:"order,omitempty"`

	// ORDER_ACCEPTED：本记录内随后将要立即成交的数量（接受时需冻结的量为 OrigQty-FilledAtAccept）。
	FilledAtAccept int64 `json:"filled_at_accept,omitempty"`

	// ORDER_REJECTED
	Reason RejectReason `json:"reason,omitempty"`
	Detail string       `json:"detail,omitempty"`

	// ORDER_FILLED
	TakerID string `json:"taker_id,omitempty"`
	MakerID string `json:"maker_id,omitempty"`
	Price   int64  `json:"price,omitempty"`
	Qty     int64  `json:"qty,omitempty"`

	// ORDER_AMENDED
	OldQty int64 `json:"old_qty,omitempty"`
	NewQty int64 `json:"new_qty,omitempty"`

	// DEPOSIT：Symbol 为空表示入金，否则为入券（计入可用持仓）。
	DepositAmount int64 `json:"deposit_amount,omitempty"`
}

// JournalRecord 一条持久化记录：一条指令（或账户开立）产生的全部事件原子落盘。
type JournalRecord struct {
	Seq    int64   `json:"seq"`
	Time   int64   `json:"time"`
	Events []Event `json:"events"`
}
