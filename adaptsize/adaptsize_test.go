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
		_ = c.Set(randKey("s", i), make([]byte, 1<<10)) // 1 KiB
		if _, ok := c.Get(randKey("s", i)); ok {
			admittedSmall++
		}
		_ = c.Set(randKey("L", i), make([]byte, 4<<20)) // 4 MiB
		if _, ok := c.Get(randKey("L", i)); ok {
			admittedLarge++
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
	_ = c.Set("a", make([]byte, 800))
	_ = c.Set("b", make([]byte, 400)) // should evict a
	if _, ok := c.Get("a"); ok {
		t.Fatal("expected a evicted")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("expected b present")
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
		_ = c.Set(k, make([]byte, 512))
		c.Get(k)
		// occasional large misses
		if i%50 == 0 {
			_ = c.Set(randKey("cold", i), make([]byte, 256<<10))
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
