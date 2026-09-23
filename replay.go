package matching

import (
	"errors"
	"fmt"
	"os"
)

// RecoverJournal 在崩溃后使用：截断流水末尾不完整（未以换行结束）的行。
// 已确认提交的完整记录保持不变；checksum 不符的损坏会被当作错误返回，
// 绝不静默丢弃。返回被截断的字节数。
func RecoverJournal(path string) (truncated int64, err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := info.Size()
	if size == 0 {
		return 0, nil
	}

	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	validEnd := int64(0)
	off := int64(0)
	for off < size {
		n, rerr := f.ReadAt(chunk, off)
		for i := 0; i < n; i++ {
			c := chunk[i]
			if c == '\n' {
				// 校验刚结束的一行。
				if len(buf) > 0 {
					if _, derr := decodeRecord(string(buf)); derr != nil {
						return 0, fmt.Errorf("journal %s offset %d: %w", path, validEnd, derr)
					}
				}
				validEnd = off + int64(i) + 1
				buf = buf[:0]
			} else {
				buf = append(buf, c)
			}
		}
		off += int64(n)
		if rerr != nil {
			break
		}
	}

	if validEnd == size {
		return 0, nil // 全部完整
	}
	// validEnd 之后是半截行：截断。
	if err := f.Truncate(validEnd); err != nil {
		return 0, err
	}
	return size - validEnd, nil
}

// ReplayFromJournal 从事件流水全新回放，重建引擎内存状态（只读打开，
// 不追加、不 fsync），因此重复回放不产生任何额外副作用。
// 若末尾存在半截写入，先按 RecoverJournal 的语义截断再回放。
func ReplayFromJournal(path string, cfg Config) (*Engine, error) {
	if _, err := RecoverJournal(path); err != nil {
		return nil, err
	}
	cfg.JournalPath = ""
	e := newEmptyEngine(cfg)
	if err := replayInto(e, path); err != nil {
		return nil, err
	}
	return e, nil
}

func replayInto(e *Engine, path string) error {
	return scanRecords(path, func(r *JournalRecord) error {
		return e.applyRecord(r)
	})
}

// applyRecord 将一条记录中的事件依次施加到内存状态。
// 实时执行（先落盘后调用）与崩溃回放共用此唯一入口，保证结论一致。
func (e *Engine) applyRecord(r *JournalRecord) error {
	if r.Seq > e.seq {
		e.seq = r.Seq
	}
	for i := range r.Events {
		ev := &r.Events[i]
		switch ev.Type {
		case EvAccountOpened:
			if err := e.ledger.openAccount(ev.Account, ev.Cash); err != nil {
				return fmt.Errorf("apply account opened: %w", err)
			}

		case EvOrderAccepted:
			o := ev.Order
			if o == nil {
				return errors.New("apply accepted: missing order")
			}
			if _, exists := e.orders[o.ID]; exists {
				return fmt.Errorf("apply accepted: duplicate order %s", o.ID)
			}
			if o.Type == Limit {
				// 接受时按原始全额冻结（此时 FilledQty 恒为 0）。
				if o.Side == Buy {
					need, _ := notional(o.Price, o.OrigQty)
					if !e.ledger.reserveCash(o.Account, need) {
						return fmt.Errorf("apply accepted: reserve cash failed for %s", o.ID)
					}
				} else {
					if !e.ledger.reservePosition(o.Account, o.Symbol, o.OrigQty) {
						return fmt.Errorf("apply accepted: reserve position failed for %s", o.ID)
					}
				}
			}
			e.orders[o.ID] = o
			if o.Type == Limit && ev.FilledAtAccept < o.OrigQty {
				// 立即部分成交的限价单先入簿，随后的 fill 事件会把已成交部分移除；
				// 立即全部成交（FilledAtAccept==OrigQty）则无需入簿。
				e.bookFor(o.Symbol).add(o)
			}
			// 注意：不在此处预置 FilledQty，成交量完全由随后的 fill 事件累加，
			// 实时与回放共用 applyFill，避免重复计数。FilledAtAccept 仅用于
			// 判断是否需要入簿。
			o.Status = StatusNew

		case EvOrderRejected:
			o := ev.Order
			if o == nil {
				return errors.New("apply rejected: missing order")
			}
			if cur, exists := e.orders[o.ID]; exists {
				// 针对已存在订单的拒绝（如撤/改已终结订单）：保留订单本体，
				// 拒绝记录本身不改变订单状态。
				_ = cur
			} else {
				o.Status = StatusRejected
				o.FilledQty = 0
				e.orders[o.ID] = o
			}

		case EvOrderFilled:
			if err := e.applyFill(ev); err != nil {
				return err
			}

		case EvOrderCancelled:
			if err := e.applyCancel(ev); err != nil {
				return err
			}

		case EvOrderAmended:
			if err := e.applyAmend(ev); err != nil {
				return err
			}

		case EvDeposit:
			if ev.Symbol == "" {
				if err := e.ledger.creditCash(ev.Account, ev.DepositAmount); err != nil {
					return fmt.Errorf("apply deposit cash: %w", err)
				}
			} else {
				if err := e.ledger.creditPosition(ev.Account, ev.Symbol, ev.DepositAmount); err != nil {
					return fmt.Errorf("apply deposit position: %w", err)
				}
			}

		default:
			return fmt.Errorf("apply: unknown event type %d", ev.Type)
		}
	}
	return nil
}

func (e *Engine) applyFill(ev *Event) error {
	taker, ok := e.orders[ev.TakerID]
	if !ok {
		return fmt.Errorf("apply fill: taker %s missing", ev.TakerID)
	}
	maker, ok := e.orders[ev.MakerID]
	if !ok {
		return fmt.Errorf("apply fill: maker %s missing", ev.MakerID)
	}

	// 被动方始终是在簿限价单：释放对应冻结并按成交价交割。
	if err := e.ledger.applyReservedFill(maker, ev.Price, ev.Qty); err != nil {
		return fmt.Errorf("apply fill maker: %w", err)
	}
	// 主动方：限价单有冻结，市价单按可用现金/持仓即时交割。
	if taker.Type == Limit {
		if err := e.ledger.applyReservedFill(taker, ev.Price, ev.Qty); err != nil {
			return fmt.Errorf("apply fill taker: %w", err)
		}
	} else {
		acct, _ := e.ledger.get(taker.Account)
		if err := e.ledger.settleSide(acct, taker.Side, taker.Symbol, ev.Price, ev.Qty); err != nil {
			return fmt.Errorf("apply fill market taker: %w", err)
		}
	}

	maker.FilledQty += ev.Qty
	taker.FilledQty += ev.Qty
	if maker.Remaining() == 0 {
		maker.Status = StatusFilled
		e.bookFor(maker.Symbol).remove(maker)
	} else {
		maker.Status = StatusPartiallyFilled
	}
	if taker.Remaining() == 0 {
		taker.Status = StatusFilled
		if taker.Type == Limit {
			e.bookFor(taker.Symbol).remove(taker)
		}
	} else {
		taker.Status = StatusPartiallyFilled
	}
	return nil
}

func (e *Engine) applyCancel(ev *Event) error {
	o := ev.Order
	if o == nil {
		return errors.New("apply cancel: missing order")
	}
	cur, ok := e.orders[o.ID]
	if !ok {
		return fmt.Errorf("apply cancel: order %s missing", o.ID)
	}
	remain := cur.Remaining()
	if cur.Type == Limit && remain > 0 {
		if cur.Side == Buy {
			need, _ := notional(cur.Price, remain)
			e.ledger.releaseCash(cur.Account, need)
		} else {
			e.ledger.releasePosition(cur.Account, cur.Symbol, remain)
		}
		e.bookFor(cur.Symbol).remove(cur)
	}
	cur.Status = StatusCancelled
	return nil
}

func (e *Engine) applyAmend(ev *Event) error {
	o := ev.Order
	if o == nil {
		return errors.New("apply amend: missing order")
	}
	cur, ok := e.orders[o.ID]
	if !ok {
		return fmt.Errorf("apply amend: order %s missing", o.ID)
	}
	delta := ev.NewQty - ev.OldQty
	if cur.Side == Buy {
		amount, _ := notional(cur.Price, delta)
		if delta > 0 {
			if !e.ledger.reserveCash(cur.Account, amount) {
				return fmt.Errorf("apply amend: reserve cash failed for %s", cur.ID)
			}
		} else if delta < 0 {
			e.ledger.releaseCash(cur.Account, -amount)
		}
	} else {
		if delta > 0 {
			if !e.ledger.reservePosition(cur.Account, cur.Symbol, delta) {
				return fmt.Errorf("apply amend: reserve position failed for %s", cur.ID)
			}
		} else if delta < 0 {
			e.ledger.releasePosition(cur.Account, cur.Symbol, -delta)
		}
	}
	cur.OrigQty = ev.NewQty
	switch {
	case cur.Remaining() == 0:
		cur.Status = StatusFilled
		e.bookFor(cur.Symbol).remove(cur)
	case cur.FilledQty > 0:
		cur.Status = StatusPartiallyFilled
	default:
		cur.Status = StatusNew
	}
	// 同价改量不改变 list.Element，队列优先级天然保留。
	return nil
}
