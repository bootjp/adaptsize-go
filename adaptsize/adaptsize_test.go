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
		if c.Request(Request{Key: randKey("s", i), SizeBytes: 1 << 10, Hit: false}) { // miss
			admittedSmall++
		}
		if c.Request(Request{Key: randKey("L", i), SizeBytes: 4 << 20, Hit: false}) { // miss
			admittedLarge++
		}
	}
	ps := float64(admittedSmall) / float64(N)
	pl := float64(admittedLarge) / float64(N)
	if !(ps > pl) {
		t.Fatalf("expected small admission>large, got ps=%.3f pl=%.3f", ps, pl)
	}
}

func TestRequestHitRecordsMetrics(t *testing.T) {
	c := newDeterministic(1 << 20)
	defer c.Close()
	if c.Request(Request{Key: "a", SizeBytes: 800, Hit: true}) { // hit should not request admission
		t.Fatal("expected hit to return false for admission")
	}
	c.winMu.Lock()
	obs := c.obs["a"]
	c.winMu.Unlock()
	if obs == nil || obs.cnt != 1 || obs.size != 800 {
		t.Fatalf("hit metrics not recorded: %+v", obs)
	}
	if c.Request(Request{Key: "a", SizeBytes: 1200, Hit: true}) {
		t.Fatal("expected hit to return false for admission")
	}
	c.winMu.Lock()
	obs = c.obs["a"]
	c.winMu.Unlock()
	if obs.cnt != 2 || obs.size != 1200 {
		t.Fatalf("hit metrics not updated: %+v", obs)
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
		_ = c.Request(Request{Key: k, SizeBytes: 512, Hit: false}) // miss then admit decision ignored here
		_ = c.Request(Request{Key: k, SizeBytes: 512, Hit: true})  // subsequent hit to record hotness
		// occasional large misses
		if i%50 == 0 {
			_ = c.Request(Request{Key: randKey("cold", i), SizeBytes: 256 << 10, Hit: false})
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

func TestRequestOversize(t *testing.T) {
	c := newDeterministic(1024)
	defer c.Close()
	admit := c.Request(Request{Key: "big", SizeBytes: 2048, Hit: false})
	if admit {
		t.Fatal("expected oversize object not to be admitted")
	}
	c.winMu.Lock()
	obs := c.obs["big"]
	c.winMu.Unlock()
	if obs == nil || obs.cnt != 1 || obs.size != 2048 {
		t.Fatalf("oversize request not recorded: %+v", obs)
	}
}

func TestBuildRatesEMA(t *testing.T) {
	c := newDeterministic(1 << 20)
	prevAOld := 10.0
	c.prevR["a"] = prevAOld
	snap := map[string]obs{
		"a": {size: 100, cnt: 2},
		"b": {size: 200, cnt: 3},
		"z": {size: 0, cnt: 5}, // should be ignored due to size 0
	}
	items, total := c.buildRates(snap)
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if total <= 0 {
		t.Fatalf("expected positive total, got %f", total)
	}
	expA := 0.5*float64(snap["a"].cnt) + 0.5*prevAOld
	expB := 0.5 * float64(snap["b"].cnt)
	if c.prevR["a"] != expA || c.prevR["b"] != expB {
		t.Fatalf("unexpected EMA values: prevR=%v", c.prevR)
	}
	rate := func(size int64) float64 {
		for _, it := range items {
			if it.s == size {
				return it.r
			}
		}
		return -1
	}
	if rate(100) != expA || rate(200) != expB {
		t.Fatalf("unexpected rates: %+v", items)
	}
}

func TestTuneOnceNoDataKeepsC(t *testing.T) {
	c := newDeterministic(1 << 20)
	defer c.Close()
	c0 := c.ParameterC()
	c.TuneOnce()
	if c.ParameterC() != c0 {
		t.Fatalf("expected c unchanged without data, got %f -> %f", c0, c.ParameterC())
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
