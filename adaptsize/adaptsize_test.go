package adaptsize

import (
	"math"
	"math/rand/v2"
	"testing"
)

func newDeterministic(capacity int64) *Cache {
	r := rand.New(rand.NewPCG(1, 0)) // seed=1, stream=0
	c := New(Options{
		CapacityBytes: capacity,
		WindowN:       1_000_000, // avoid background tuning in most tests
		Alpha:         0.5,
		GridMin:       1 << 10,
		GridMax:       64 << 20,
		GridSteps:     16,
		Rand:          r,
	})
	return c
}

func TestAdmissionMonotonic(t *testing.T) {
	c := newDeterministic(1 << 60) // practically infinite
	defer c.Close()
	// force c = 1 MiB
	c.cBits.Store(math.Float64bits(1 << 20))
	N := 20000
	admittedSmall := 0
	admittedLarge := 0
	for i := 0; i < N; i++ {
		admitted, _ := c.Store(randKey("s", i), 1<<10) // 1 KiB
		if admitted {
			admittedSmall++
		}
		if c.Get(randKey("s", i)) {
			// track metrics
		}
		admitted, _ = c.Store(randKey("L", i), 4<<20) // 4 MiB
		if admitted {
			admittedLarge++
		}
		if c.Get(randKey("L", i)) {
			// track metrics
		}
	}
	ps := float64(admittedSmall) / float64(N)
	pl := float64(admittedLarge) / float64(N)
	if !(ps > pl) {
		t.Fatalf("expected small admission>large, got ps=%.3f pl=%.3f", ps, pl)
	}
}

func TestLRUEviction(t *testing.T) {
	c := newDeterministic(1024) // 1 KiB
	defer c.Close()
	c.cBits.Store(math.Float64bits(1 << 30)) // admit almost always
	_, evicted1 := c.Store("a", 800)
	if len(evicted1) > 0 {
		t.Fatalf("unexpected evictions: %v", evicted1)
	}
	_, evicted2 := c.Store("b", 400) // should evict a
	if c.Get("a") {
		t.Fatal("expected a evicted")
	}
	if !c.Get("b") {
		t.Fatal("expected b present")
	}
	// Check that "a" was in the evicted list
	found := false
	for _, key := range evicted2 {
		if key == "a" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected 'a' to be in evicted list")
	}
}

func TestBackgroundTuningMovesC(t *testing.T) {
	r := rand.New(rand.NewPCG(2, 0)) // seed=1, stream=0
	c := New(Options{
		CapacityBytes: 1 << 20, // 1 MiB
		WindowN:       5000,
		GridMin:       256,
		GridMax:       8 << 20,
		GridSteps:     12,
		Rand:          r,
	})
	defer c.Close()
	c.cBits.Store(math.Float64bits(4 << 20)) // start very large: 4 MiB, optimal should be smaller on small-object workload
	c0 := c.ParameterC()

	// Workload: many small objects hot, few large cold.
	for i := 0; i < 30_000; i++ {
		// small hot keys cycle
		k := randKey("hot", i%128)
		_, _ = c.Store(k, 512)
		c.Get(k)
		// occasional large misses
		if i%50 == 0 {
			_, _ = c.Store(randKey("cold", i), 256<<10)
		}
	}
	// give time for tuner to run
	c.TuneOnce()
	c1 := c.ParameterC()
	if c1 >= c0 {
		t.Fatalf("expected c to change, stayed at %f", c0)
	}
	if math.IsNaN(c1) || math.IsInf(c1, 0) {
		t.Fatalf("c invalid: %f", c1)
	}
}

func randKey(prefix string, i int) string { return prefix + "-" + strconvI(i) }

func strconvI(i int) string {
	// simple fast int->string
	return string([]byte{
		byte('0' + (i/10000)%10),
		byte('0' + (i/1000)%10),
		byte('0' + (i/100)%10),
		byte('0' + (i/10)%10),
		byte('0' + (i)%10),
	})
}
