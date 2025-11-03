package adaptsize

import (
	"container/list"
	crand "crypto/rand"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

// Options controls cache behavior.
type Options struct {
	CapacityBytes int64      // K: total capacity in bytes
	WindowN       int        // Δ: requests per tuning round (default 250k)
	Alpha         float64    // EMA factor for rates r_i (default 0.5)
	GridMin       int64      // min c in bytes (default 1 KiB)
	GridMax       int64      // max c in bytes (default 64 MiB)
	GridSteps     int        // number of c candidates, log-spaced (default 32)
	Rand          *rand.Rand // RNG for admission; default seeded
}

type Cache struct {
	opts Options

	mu    sync.RWMutex
	used  int64
	items map[string]*entry
	lru   *list.List

	// parameter c stored atomically
	cBits atomic.Uint64

	// tuning window
	winMu   sync.Mutex
	winReqs int
	obs     map[string]*obs
	prevR   map[string]float64 // EMA state

	// background tuning
	tuneCh chan struct{}
	stopCh chan struct{}
}

type entry struct {
	key  string
	val  []byte
	size int64
	node *list.Element
}

type obs struct {
	size int64
	cnt  int64
}

func defaultRandom() *rand.PCG {
	var s1, s2 uint64
	if err := binary.Read(crand.Reader, binary.LittleEndian, &s1); err != nil {
		// fallback
		//nolint:gosec
		s1 = uint64(time.Now().UnixNano())
	}
	if err := binary.Read(crand.Reader, binary.LittleEndian, &s2); err != nil {
		s2 = s1 ^ 0x9e3779b97f4a7c15
	}
	return rand.NewPCG(s1, s2)
}

// New constructs a cache and starts the background tuner.
func New(opts Options) *Cache {
	if opts.WindowN <= 0 {
		opts.WindowN = 250_000
	}
	if opts.Alpha <= 0 || opts.Alpha > 1 {
		opts.Alpha = 0.5
	}
	if opts.GridMin <= 0 {
		opts.GridMin = 1 << 10
	} // 1 KiB
	if opts.GridMax <= opts.GridMin {
		opts.GridMax = 64 << 20
	} // 64 MiB
	if opts.GridSteps <= 1 {
		opts.GridSteps = 32
	}
	if opts.Rand == nil {
		//nolint:gosec
		opts.Rand = rand.New(defaultRandom())
	}

	c := &Cache{
		opts:   opts,
		items:  make(map[string]*entry),
		lru:    list.New(),
		obs:    make(map[string]*obs),
		prevR:  make(map[string]float64),
		tuneCh: make(chan struct{}, 1),
		stopCh: make(chan struct{}),
	}
	c.setC(256 << 10) // initial c = 256 KiB
	go c.tuneLoop()
	return c
}

// Close stops background tuning.
func (c *Cache) Close() { close(c.stopCh) }

// Len returns number of cached entries.
func (c *Cache) Len() int { c.mu.RLock(); defer c.mu.RUnlock(); return len(c.items) }

// UsedBytes returns used capacity.
func (c *Cache) UsedBytes() int64 { c.mu.RLock(); defer c.mu.RUnlock(); return c.used }

// ParameterC returns current c.
func (c *Cache) ParameterC() float64 { return math.Float64frombits(c.cBits.Load()) }

// Get returns value and ok. Touches LRU and records stats.
func (c *Cache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.lru.MoveToFront(e.node)
		c.record(key, e.size)
		return e.val, true
	}
	c.record(key, 0)
	return nil, false
}

// Set inserts or updates value. Admission is probabilistic.
func (c *Cache) Set(key string, value []byte) error {
	size := int64(len(value))
	if size > c.opts.CapacityBytes {
		return nil // never admit larger than capacity
	}
	// admission using atomic c
	cVal := math.Float64frombits(c.cBits.Load())
	admit := c.opts.Rand.Float64() < math.Exp(-float64(size)/cVal)
	if !admit {
		c.record(key, size)
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if e, ok := c.items[key]; ok {
		c.used += size - e.size
		e.val = value
		e.size = size
		c.lru.MoveToFront(e.node)
	} else {
		for c.used+size > c.opts.CapacityBytes {
			c.evictOne()
		}
		n := c.lru.PushFront(&entry{key: key})
		e := &entry{key: key, val: value, size: size, node: n}
		n.Value = e
		c.items[key] = e
		c.used += size
	}
	// stats
	c.record(key, size)
	return nil
}

func (c *Cache) evictOne() {
	if c.lru.Len() == 0 {
		return
	}
	b := c.lru.Back()
	if b == nil {
		return
	}
	e, ok := b.Value.(*entry)
	if !ok {
		return
	}
	delete(c.items, e.key)
	c.used -= e.size
	c.lru.Remove(b)
}

func (c *Cache) record(key string, size int64) {
	c.winMu.Lock()
	o := c.obs[key]
	if o == nil {
		o = &obs{}
		c.obs[key] = o
	}
	if size > 0 {
		o.size = size
	}
	o.cnt++
	c.winReqs++
	need := c.winReqs >= c.opts.WindowN
	c.winMu.Unlock()

	if need {
		select {
		case c.tuneCh <- struct{}{}:
		default:
		}
	}
}

func (c *Cache) setC(v int64) { c.cBits.Store(math.Float64bits(float64(v))) }
