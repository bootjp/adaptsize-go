
package adaptsize

import (
	"math"
)

// background tuner loop
func (c *Cache) tuneLoop() {
	for {
		select {
		case <-c.tuneCh:
			// snapshot window
			c.winMu.Lock()
			snap := make(map[string]obs, len(c.obs))
			for k, v := range c.obs { snap[k] = *v }
			c.obs = make(map[string]*obs)
			c.winReqs = 0
			c.winMu.Unlock()

			if len(snap) == 0 { continue }
			items, totalReq := c.buildRates(snap)
			if len(items) == 0 { continue }
			bestC := c.searchBestC(items, totalReq)
			if !math.IsNaN(bestC) && !math.IsInf(bestC, 0) {
				c.cBits.Store(math.Float64bits(bestC))
			}
		case <-c.stopCh:
			return
		}
	}
}

type rateItem struct{ s int64; r float64 }

func (c *Cache) buildRates(snap map[string]obs) ([]rateItem, float64) {
	items := make([]rateItem, 0, len(snap))
	total := 0.0
	for k, o := range snap {
		if o.size <= 0 { continue }
		prev := c.prevR[k]
		rate := c.opts.Alpha*float64(o.cnt) + (1.0-c.opts.Alpha)*prev
		c.prevR[k] = rate
		items = append(items, rateItem{s: o.size, r: rate})
		total += rate
	}
	return items, total
}

func (c *Cache) searchBestC(items []rateItem, totalReq float64) float64 {
	// log-spaced grid for c
	steps := c.opts.GridSteps
	grid := make([]float64, steps)
	logMin := math.Log(float64(c.opts.GridMin))
	logMax := math.Log(float64(c.opts.GridMax))
	for i := 0; i < steps; i++ {
		t := float64(i) / float64(steps-1)
		grid[i] = math.Exp(logMin + t*(logMax-logMin))
	}

	bestC := math.Float64frombits(c.cBits.Load())
	best := -1.0
	for _, cand := range grid {
		mu := solveMu(items, cand, c.opts.CapacityBytes)
		if mu <= 0 || math.IsNaN(mu) || math.IsInf(mu, 0) { continue }
		hits := 0.0
		for _, it := range items {
			p := pinClosedForm(it.r, mu, float64(it.s), cand)
			hits += it.r * p
		}
		ohr := hits / totalReq
		if ohr > best {
			best, bestC = ohr, cand
		}
	}
	return bestC
}

// P_in(i) closed form.
func pinClosedForm(ri, mu float64, si float64, c float64) float64 {
	if ri <= 0 { return 0 }
	x := math.Exp(ri/mu) - 1.0
	e := math.Exp(-si / c)
	num := x * e
	return num / (1.0 + num)
}

// Solve μ: sum P_in(i)*s_i = K via monotone binary search.
func solveMu(items []rateItem, c float64, K int64) float64 {
	if K <= 0 { return math.NaN() }
	muLo := 1e-6
	muHi := 1.0
	for i := 0; i < 40; i++ {
		if capBytes(items, muHi, c) < float64(K) { break }
		muHi *= 2
	}
	for i := 0; i < 60; i++ {
		mid := 0.5*(muLo+muHi)
		sum := capBytes(items, mid, c)
		if sum > float64(K) {
			muLo = mid
		} else {
			muHi = mid
		}
	}
	return 0.5*(muLo+muHi)
}

func capBytes(items []rateItem, mu, c float64) float64 {
	sum := 0.0
	for _, it := range items {
		p := pinClosedForm(it.r, mu, float64(it.s), c)
		sum += p * float64(it.s)
	}
	return sum
}


/*
TuneOnce runs one tuning round synchronously:
- snapshots the window
- recomputes EMA rates
- grid-searches c and solves μ per candidate
- installs the best c
It is safe to call concurrently with Get/Set.
*/
func (c *Cache) TuneOnce() {
	// snapshot window
	c.winMu.Lock()
	snap := make(map[string]obs, len(c.obs))
	for k, v := range c.obs { snap[k] = *v }
	c.obs = make(map[string]*obs)
	c.winReqs = 0
	c.winMu.Unlock()

	if len(snap) == 0 { return }
	items, totalReq := c.buildRates(snap)
	if len(items) == 0 || totalReq == 0 { return }
	bestC := c.searchBestC(items, totalReq)
	if !math.IsNaN(bestC) && !math.IsInf(bestC, 0) {
		c.cBits.Store(math.Float64bits(bestC))
	}
}
