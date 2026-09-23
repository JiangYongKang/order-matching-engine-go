package matching

import "fmt"

// crosses 判断 maker 价位是否与主动单可成交。
func crosses(side Side, makerPrice, limitPrice int64, isMarket bool) bool {
	if isMarket {
		return true
	}
	if side == Buy {
		return makerPrice <= limitPrice
	}
	return makerPrice >= limitPrice
}

// plannedFill 一次吃单成交计划。stpCancel 为 true 时表示这不是成交，
// 而是一笔因自成交被撤销的同账户挂单（qty 恒为 0，只有 makerEv）。
type plannedFill struct {
	takerID   string
	makerID   string
	price     int64
	qty       int64
	stpCancel bool
	makerEv   Event // 被动方成交事件或 STP 撤销事件
}

// matchPlan 撮合规划结果。规划阶段不修改任何账户与订单簿状态：
// 真正的状态变更由 applyRecord 按事件统一施加，实时与回放共用同一路径。
type matchPlan struct {
	fills        []plannedFill
	totalFilled  int64
	rejectReason RejectReason
	rejectDetail string
	stopReason   RejectReason // 市价单部分成交/零成交时的终止原因
}

// match 执行价格-时间优先撮合（纯规划，读状态但不改状态）。
//
// 自成交：在触及对手盘第一个同账户挂单时判定：
//   - STPRejectTaker：主动单整体拒绝（此前规划的成交一并作废，无中间状态）；
//   - STPCancelMaker：撤销该同账户挂单（事件中释放其冻结），主动单继续向下。
//
// 市价单可成交量还受账户可用现金（买）/可用持仓（卖）以及单标的持仓上限约束；
// 可承受量为 0 时停止，剩余按 IOC 撤销或整单拒绝。
func (e *Engine) match(taker *Order, isMarket bool) *matchPlan {
	plan := &matchPlan{}
	b := e.bookFor(taker.Symbol)
	filledSoFar := map[string]int64{} // maker 本轮已规划成交量
	remain := taker.Remaining()

	cashBudget := int64(0)
	posBudget := int64(0)
	posCapBudget := int64(-1) // -1 表示不限
	if isMarket {
		acct, _ := e.ledger.get(taker.Account)
		if taker.Side == Buy {
			cashBudget = acct.Cash
			if e.limits.MaxSymbolPosition > 0 {
				net := e.symbolNetPosition(taker.Account, taker.Symbol)
				posCapBudget = e.limits.MaxSymbolPosition - net
				if posCapBudget < 0 {
					posCapBudget = 0
				}
			}
		} else {
			posBudget = acct.Positions[taker.Symbol]
		}
	}

	// 沿对手盘价位序列遍历，同价位严格按时间优先（队列从前到后）。
	levels := b.askPx
	levelMap := b.asks
	if taker.Side == Sell {
		levels = b.bidPx
		levelMap = b.bids
	}

levelLoop:
	for _, px := range levels {
		if remain == 0 {
			break
		}
		lvl := levelMap[px]
		if !crosses(taker.Side, px, taker.Price, isMarket) {
			plan.stopReason = RejNoLiquidity
			break
		}
		for el := lvl.queue.Front(); el != nil; el = el.Next() {
			if remain == 0 {
				break levelLoop
			}
			maker := el.Value.(*Order)
			makerRemain := maker.Remaining() - filledSoFar[maker.ID]
			if makerRemain <= 0 {
				continue
			}

			if maker.Account == taker.Account {
				switch e.stp {
				case STPRejectTaker:
					// 主动单整体拒绝：此前仅为规划，丢弃即可，内存零改动。
					return &matchPlan{
						rejectReason: RejSelfTrade,
						rejectDetail: fmt.Sprintf("self-trade with resting order %s (account=%s)",
							maker.ID, maker.Account),
					}
				case STPCancelMaker:
					plan.fills = append(plan.fills, plannedFill{
						takerID: taker.ID, makerID: maker.ID, price: px, stpCancel: true,
						makerEv: Event{
							Type: EvOrderCancelled, Symbol: taker.Symbol, Order: maker,
							Reason: RejSelfTrade,
							Detail: fmt.Sprintf("STP cancel-maker against taker %s", taker.ID),
						},
					})
					filledSoFar[maker.ID] = maker.Remaining() // 视为消费完，跳过
					continue
				}
			}

			qty := makerRemain
			if qty > remain {
				qty = remain
			}
			if isMarket && taker.Side == Buy {
				afford := cashBudget / px
				if posCapBudget >= 0 && afford > posCapBudget {
					afford = posCapBudget
				}
				if afford <= 0 {
					plan.stopReason = RejInsufficientFunds
					break levelLoop
				}
				if qty > afford {
					qty = afford
				}
			}
			if isMarket && taker.Side == Sell {
				if posBudget <= 0 {
					plan.stopReason = RejInsufficientPosition
					break levelLoop
				}
				if qty > posBudget {
					qty = posBudget
				}
			}

			plan.fills = append(plan.fills, plannedFill{
				takerID: taker.ID, makerID: maker.ID, price: px, qty: qty,
				makerEv: Event{
					Type: EvOrderFilled, Symbol: taker.Symbol,
					TakerID: taker.ID, MakerID: maker.ID, Price: px, Qty: qty,
				},
			})
			plan.totalFilled += qty
			remain -= qty
			filledSoFar[maker.ID] += qty
			if isMarket {
				if taker.Side == Buy {
					cashBudget -= px * qty
					if posCapBudget >= 0 {
						posCapBudget -= qty
					}
				} else {
					posBudget -= qty
				}
			}
		}
	}
	if remain > 0 && plan.rejectReason == RejNone {
		// 区分终止原因：预算耗尽优先，否则为无流动性。
		if isMarket {
			switch {
			case taker.Side == Buy && plan.stopReason == RejInsufficientFunds:
			case taker.Side == Sell && plan.stopReason == RejInsufficientPosition:
			default:
				plan.stopReason = RejNoLiquidity
			}
		} else {
			plan.stopReason = RejNone // 限价单剩余正常挂簿
		}
	}
	return plan
}

func (e *Engine) bookFor(symbol string) *Book {
	b, ok := e.books[symbol]
	if !ok {
		b = newBook(symbol)
		e.books[symbol] = b
	}
	return b
}
