package adaptsize

import (
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

type obs struct {
	size int64
	cnt  int64
}

// Request holds metrics about a cache access. The caller is responsible for
// determining whether it was a hit in their underlying cache.
type Request struct {
	Key       string
	SizeBytes int64
	Hit       bool
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
func (c *Cache) Close() {
	close(c.stopCh)
}

// ParameterC returns current c.
func (c *Cache) ParameterC() float64 {
	return math.Float64frombits(c.cBits.Load())
}

// Request records a cache request and returns whether a miss should be
// admitted. On misses (Hit=false), the return value is the probabilistic
// admission decision based on exp(-size/c).
func (c *Cache) Request(req Request) bool {
	c.record(req.Key, req.SizeBytes)
	if req.Hit {
		return false
	}
	if req.SizeBytes > c.opts.CapacityBytes {
		return false // never admit larger than capacity
	}
	// admission using atomic c
	cVal := math.Float64frombits(c.cBits.Load())
	return c.opts.Rand.Float64() < math.Exp(-float64(req.SizeBytes)/cVal)
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

func (c *Cache) setC(v int64) {
	c.cBits.Store(math.Float64bits(float64(v)))
}
