package matching

import "fmt"

// RiskLimits 风险限额，0 表示不限制。
//
// 判定语义（均在指令改变订单簿/账户之前判定，触发即整体拒绝、无中间状态）：
//   - MaxAccountBuyNotional：单账户当前在簿买单冻结名义金额（含本单全额）的上限。
//     仅对买入限价单生效；卖出单不占用买单额度；市价单即时成交不挂簿、不占用。
//   - MaxSymbolPosition：单账户单一标的提交后理论最大持仓绝对值的上限。
//     买单按「当前净持仓 + 本单数量」判定（资金侧已保证现金足额，故按最坏情形）；
//     卖单提交时持仓已足额冻结，卖单不会使持仓绝对值超过当前值，故必然通过。
//   - MaxGlobalExposure：全引擎在簿挂单双边名义金额（含本单全额）的上限，买卖合计。
type RiskLimits struct {
	MaxAccountBuyNotional int64
	MaxSymbolPosition     int64
	MaxGlobalExposure     int64
}

// riskView 风险判定所需的计数器视图，由引擎提供。
type riskView struct {
	accountBuyNotional func(account string) int64
	symbolPosition     func(account, symbol string) int64
	globalExposure     func() int64
}

// riskCheckDetail 生成可解释的限额判定描述。
func riskCheckDetail(r RejectReason, o *Order, current, add, limit int64) string {
	return fmt.Sprintf("%s: current=%d add=%d limit=%d (account=%s symbol=%s order=%s)",
		r, current, add, limit, o.Account, o.Symbol, o.ID)
}

// checkRisk 校验限价单提交是否满足三类限额，返回拒单原因（RejNone 表示通过）。
// 所有判定均按「当前值 + 本单全额增量」做提交前检查，保证不会产生部分生效。
func checkRisk(limits RiskLimits, o *Order, view riskView) RejectReason {
	add, ok := notional(o.Price, o.Remaining())
	if !ok {
		return RejInvalidPrice
	}
	if o.Side == Buy && limits.MaxAccountBuyNotional > 0 {
		cur := view.accountBuyNotional(o.Account)
		if cur+add > limits.MaxAccountBuyNotional {
			return RejRiskAccountLimit
		}
	}
	if limits.MaxSymbolPosition > 0 {
		cur := view.symbolPosition(o.Account, o.Symbol)
		if o.Side == Buy && absInt64(cur)+o.Remaining() > limits.MaxSymbolPosition {
			return RejRiskSymbolLimit
		}
	}
	if limits.MaxGlobalExposure > 0 {
		cur := view.globalExposure()
		if cur+add > limits.MaxGlobalExposure {
			return RejRiskGlobalLimit
		}
	}
	return RejNone
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
