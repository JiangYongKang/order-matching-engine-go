package engine

import "sort"

// Book is the resting order book for one symbol. It is single-writer:
// the owning symbol shard serializes all access.
//
// At each price, orders are stored in strict time-priority order, where
// "time" is the global monotonic PrioritySeq assigned at acceptance (lower
// sequence = earlier = higher priority).
type Book struct {
	symbol string
	bids   map[int64][]*Order // price -> time-priority ordered resting orders
	asks   map[int64][]*Order
}

// NewBook creates an empty book for a symbol.
func NewBook(symbol string) *Book {
	return &Book{
		symbol: symbol,
		bids:   make(map[int64][]*Order),
		asks:   make(map[int64][]*Order),
	}
}

// side returns the price-level map for a side.
func (b *Book) side(side Side) map[int64][]*Order {
	if side == SideBuy {
		return b.bids
	}
	return b.asks
}

// Add inserts an active order at the tail of its price level, i.e. after all
// orders that previously had priority there.
func (b *Book) Add(o *Order) {
	lv := b.side(o.Side)
	lv[o.Price] = append(lv[o.Price], o)
}

// Remove deletes the resting order from its price level. It reports whether
// the order was present.
func (b *Book) Remove(o *Order) bool {
	lv := b.side(o.Side)
	orders := lv[o.Price]
	idx := -1
	for i, x := range orders {
		if x.ID == o.ID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	lv[o.Price] = append(orders[:idx], orders[idx+1:]...)
	if len(lv[o.Price]) == 0 {
		delete(lv, o.Price)
	}
	return true
}

// bestPrice returns the best price for a resting side (highest bid / lowest
// ask) and whether any level exists.
func (b *Book) bestPrice(side Side) (int64, bool) {
	var best int64
	found := false
	for p := range b.side(side) {
		if !found {
			best, found = p, true
			continue
		}
		if side == SideBuy && p > best {
			best = p
		}
		if side == SideSell && p < best {
			best = p
		}
	}
	return best, found
}

// BestBid / BestAsk expose the touch prices.
func (b *Book) BestBid() (int64, bool) { return b.bestPrice(SideBuy) }
func (b *Book) BestAsk() (int64, bool) { return b.bestPrice(SideSell) }

// FillCandidate is one resting order an incoming order can match.
type FillCandidate struct {
	Order *Order
	Price int64
}

// crosses reports whether an incoming order with side/limit can trade at p.
// Market orders (hasLimit == false) cross any price.
func crosses(side Side, p int64, limitPrice int64, hasLimit bool) bool {
	if !hasLimit {
		return true
	}
	if side == SideBuy {
		return p <= limitPrice
	}
	return p >= limitPrice
}

// sortedOpposite returns opposing-side prices in the order they must be
// consumed: asks ascending (cheapest first) for an incoming buy, bids
// descending (highest first) for an incoming sell.
func (b *Book) sortedOpposite(side Side) []int64 {
	var prices []int64
	if side == SideBuy {
		for p := range b.asks {
			prices = append(prices, p)
		}
		sort.Slice(prices, func(i, j int) bool { return prices[i] < prices[j] })
	} else {
		for p := range b.bids {
			prices = append(prices, p)
		}
		sort.Slice(prices, func(i, j int) bool { return prices[i] > prices[j] })
	}
	return prices
}

// MatchPlan walks the opposing side in price-time priority and returns the
// sequence of resting orders an incoming order would cross, bounded by qty.
// Book state is not modified; time-priority order inside a level is the slice
// order maintained by Add.
func (b *Book) MatchPlan(side Side, limitPrice int64, qty int64, hasLimit bool) []FillCandidate {
	remaining := qty
	var out []FillCandidate
	for _, p := range b.sortedOpposite(side) {
		if !crosses(side, p, limitPrice, hasLimit) {
			break // prices are ordered worst-first; no later level can cross
		}
		for _, o := range b.side(opposite(side))[p] {
			if remaining <= 0 {
				return out
			}
			take := o.OpenQty
			if take > remaining {
				take = remaining
			}
			if take <= 0 {
				continue
			}
			out = append(out, FillCandidate{Order: o, Price: p})
			remaining -= take
		}
	}
	return out
}

func opposite(s Side) Side {
	if s == SideBuy {
		return SideSell
	}
	return SideBuy
}

// ApplyTrade consumes filledQty from a resting order (capped at its open
// quantity), removes it from the book when fully filled, and returns the
// quantity actually consumed.
func (b *Book) ApplyTrade(o *Order, filledQty int64) int64 {
	if filledQty > o.OpenQty {
		filledQty = o.OpenQty
	}
	if filledQty <= 0 {
		return 0
	}
	o.OpenQty -= filledQty
	o.FilledQty += filledQty
	if o.OpenQty == 0 {
		b.Remove(o)
	}
	return filledQty
}

// levelQty sums open quantity at one price level.
func levelQty(orders []*Order) int64 {
	var q int64
	for _, o := range orders {
		q += o.OpenQty
	}
	return q
}

// Snapshot returns a deterministic, price-sorted view of the book.
func (b *Book) Snapshot() BookSnapshot {
	snap := BookSnapshot{Symbol: b.symbol}
	bids := b.sortedOpposite(SideSell) // bids, descending
	sort.Slice(bids, func(i, j int) bool { return bids[i] > bids[j] })
	for _, p := range bids {
		if q := levelQty(b.bids[p]); q > 0 {
			snap.Bids = append(snap.Bids, BookLevel{Price: p, Qty: q})
		}
	}
	asks := b.sortedOpposite(SideBuy) // asks, ascending
	for _, p := range asks {
		if q := levelQty(b.asks[p]); q > 0 {
			snap.Asks = append(snap.Asks, BookLevel{Price: p, Qty: q})
		}
	}
	return snap
}

// Orders returns all active resting orders.
func (b *Book) Orders() []*Order {
	var out []*Order
	for _, lv := range b.bids {
		out = append(out, lv...)
	}
	for _, lv := range b.asks {
		out = append(out, lv...)
	}
	return out
}

// HasAccount reports whether any active order on either side belongs to acct.
func (b *Book) HasAccount(acct string) bool {
	for _, o := range b.Orders() {
		if o.Account == acct {
			return true
		}
	}
	return false
}
