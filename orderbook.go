package matching

import "container/list"

// priceLevel 同一价位的订单队列，严格按 Seq（时间优先）排列。
type priceLevel struct {
	price int64
	queue *list.List // 元素为 *Order
}

func newPriceLevel(price int64) *priceLevel {
	return &priceLevel{price: price, queue: list.New()}
}

// Book 单一标的的订单簿。买盘按价格降序、卖盘按价格升序排列。
type Book struct {
	symbol string
	bids   map[int64]*priceLevel
	asks   map[int64]*priceLevel
	bidPx  []int64 // 有序价位缓存（降序）
	askPx  []int64 // 有序价位缓存（升序）
	index  map[string]*list.Element
}

func newBook(symbol string) *Book {
	return &Book{
		symbol: symbol,
		bids:   map[int64]*priceLevel{},
		asks:   map[int64]*priceLevel{},
		index:  map[string]*list.Element{},
	}
}

func (b *Book) sideMap(side Side) (map[int64]*priceLevel, *[]int64) {
	if side == Buy {
		return b.bids, &b.bidPx
	}
	return b.asks, &b.askPx
}

// insertPrice 向有序价位切片插入新价位。
func insertPrice(prices *[]int64, price int64, descending bool) {
	i := 0
	if descending {
		for i < len(*prices) && (*prices)[i] > price {
			i++
		}
	} else {
		for i < len(*prices) && (*prices)[i] < price {
			i++
		}
	}
	*prices = append(*prices, 0)
	copy((*prices)[i+1:], (*prices)[i:])
	(*prices)[i] = price
}

func removePrice(prices *[]int64, price int64) {
	for i, p := range *prices {
		if p == price {
			*prices = append((*prices)[:i], (*prices)[i+1:]...)
			return
		}
	}
}

// add 将订单追加到对应价位队尾（获得该价位最低时间优先级）。
func (b *Book) add(o *Order) {
	m, px := b.sideMap(o.Side)
	lvl, ok := m[o.Price]
	if !ok {
		lvl = newPriceLevel(o.Price)
		m[o.Price] = lvl
		insertPrice(px, o.Price, o.Side == Buy)
	}
	el := lvl.queue.PushBack(o)
	b.index[o.ID] = el
}

// remove 在不改变其余订单队列位置的前提下删除订单，空价位一并清理。
func (b *Book) remove(o *Order) bool {
	el, ok := b.index[o.ID]
	if !ok {
		return false
	}
	m, px := b.sideMap(o.Side)
	lvl := m[o.Price]
	lvl.queue.Remove(el)
	delete(b.index, o.ID)
	if lvl.queue.Len() == 0 {
		delete(m, o.Price)
		removePrice(px, o.Price)
	}
	return true
}

func (b *Book) bestBid() *priceLevel {
	if len(b.bidPx) == 0 {
		return nil
	}
	return b.bids[b.bidPx[0]]
}

func (b *Book) bestAsk() *priceLevel {
	if len(b.askPx) == 0 {
		return nil
	}
	return b.asks[b.askPx[0]]
}

func (b *Book) getOrder(id string) *Order {
	if el, ok := b.index[id]; ok {
		return el.Value.(*Order)
	}
	return nil
}

func (b *Book) depth() (bidLevels, askLevels int) {
	return len(b.bidPx), len(b.askPx)
}

// openExposure 返回簿上全部挂单剩余量的名义金额合计（全局敞口风控用）。
func (b *Book) openExposure() int64 {
	var total int64
	for _, lvl := range b.bids {
		total += levelExposure(lvl)
	}
	for _, lvl := range b.asks {
		total += levelExposure(lvl)
	}
	return total
}

func levelExposure(lvl *priceLevel) int64 {
	var total int64
	for el := lvl.queue.Front(); el != nil; el = el.Next() {
		o := el.Value.(*Order)
		if n, ok := notional(o.Price, o.Remaining()); ok {
			total += n
		}
	}
	return total
}
