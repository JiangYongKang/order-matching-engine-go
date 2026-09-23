package matching

// Account 账户状态。金额与数量均为整数单位。
type Account struct {
	ID           string
	Cash         int64            // 可用现金
	ReservedCash int64            // 买单冻结金额
	Positions    map[string]int64 // 可用持仓
	ReservedPos  map[string]int64 // 卖单冻结持仓
}

// AccountSnapshot 账户状态快照。
type AccountSnapshot struct {
	ID           string           `json:"id"`
	Cash         int64            `json:"cash"`
	ReservedCash int64            `json:"reserved_cash"`
	Positions    map[string]int64 `json:"positions"`
	ReservedPos  map[string]int64 `json:"reserved_pos"`
}
