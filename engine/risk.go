package engine

// RiskChecker validates account-level, symbol-level and global exposure
// constraints. Checks are pure reads over ledger state and run before any
// state mutation, so a rejected instruction cannot leave partial effects.
//
// Valuation semantics (deterministic and explainable):
//   - notional of an order = valuationPrice * qty, always using the full
//     original quantity (worst case). Limit orders use their limit price;
//     market orders use the current best opposite price.
//   - gross exposure = sum of notional over all resting orders.
//   - global exposure = gross exposure summed over all accounts.
//   - symbol exposure = net position + net resting qty (buy +, sell -);
//     the constraint is on its absolute value.
//
// Limits with value <= 0 mean "unlimited".
type RiskChecker struct {
	cfg *RiskConfig
}

// NewRiskChecker creates a checker from configuration.
func NewRiskChecker(cfg *RiskConfig) *RiskChecker {
	return &RiskChecker{cfg: cfg}
}

// riskInputs summarizes the state needed for the checks.
type riskInputs struct {
	account        string
	symbol         string
	side           Side
	price          int64 // valuation price (limit price / best opposite for market)
	qty            int64 // full original order quantity (single-order & position checks)
	restQty        int64 // quantity that will actually rest after matching (gross exposure checks)
	ledger         *Ledger
	replacingOrder *Order // order being amended (its notional/qty is excluded)
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Check returns a RejectError when any limit would be breached.
func (c *RiskChecker) Check(in riskInputs) *RejectError {
	orderNotional := in.price * in.qty
	sign := int64(1)
	if in.side == SideSell {
		sign = -1
	}

	// Account-level limits.
	if lim := c.cfg.LimitsFor(in.account); lim != nil {
		if lim.MaxSingleOrderNotional > 0 && orderNotional > lim.MaxSingleOrderNotional {
			return rejectf(RejectRiskSingleOrder,
				"order notional %d (price=%d qty=%d) exceeds per-account single-order limit %d for %s",
				orderNotional, in.price, in.qty, lim.MaxSingleOrderNotional, in.account)
		}

		gross := in.ledger.openNotional(in.account)
		if in.replacingOrder != nil {
			gross -= in.replacingOrder.Price * in.replacingOrder.OpenQty
		}
		newResting := in.price * in.restQty
		if lim.MaxGrossExposure > 0 && gross+newResting > lim.MaxGrossExposure {
			return rejectf(RejectRiskGrossExposure,
				"account %s gross exposure %d + resting notional %d = %d exceeds limit %d",
				in.account, gross, newResting, gross+newResting, lim.MaxGrossExposure)
		}

		if maxPos, ok := lim.MaxSymbolPosition[in.symbol]; ok && maxPos > 0 {
			netOpen := in.ledger.accountNetOpenQty(in.account, in.symbol)
			if in.replacingOrder != nil && in.replacingOrder.Symbol == in.symbol {
				if in.replacingOrder.Side == SideBuy {
					netOpen -= in.replacingOrder.OpenQty
				} else {
					netOpen += in.replacingOrder.OpenQty
				}
			}
			projected := in.ledger.Position(in.account, in.symbol) + netOpen + sign*in.qty
			if absInt64(projected) > maxPos {
				return rejectf(RejectRiskSymbolPosition,
					"account %s projected %s position %d (position+open%+d order%+d) exceeds limit %d",
					in.account, in.symbol, projected,
					netOpen, sign*in.qty, maxPos)
			}
		}
	}

	// Global exposure limit.
	if c.cfg != nil && c.cfg.GlobalMaxOpenNotional > 0 {
		g := in.ledger.globalOpenNotional()
		if in.replacingOrder != nil {
			g -= in.replacingOrder.Price * in.replacingOrder.OpenQty
		}
		newResting := in.price * in.restQty
		if g+newResting > c.cfg.GlobalMaxOpenNotional {
			return rejectf(RejectRiskGlobalExposure,
				"global open notional %d + resting notional %d = %d exceeds limit %d",
				g, newResting, g+newResting, c.cfg.GlobalMaxOpenNotional)
		}
	}
	return nil
}
