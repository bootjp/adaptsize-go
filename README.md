
# AdaptSize cache for Go KVS

- Admission: `P(admit) = exp(-size/c)`
- Eviction: LRU
- Background tuning: maximize OHR by grid-searching `c`; for each `c`, solve `μ` s.t. `Σ P_in(i)*s_i = K` via binary search.

## Usage

```go
cache := adaptsize.New(adaptsize.Options{
    CapacityBytes: 1<<30, // 1 GiB
    WindowN:       250_000,
})
defer cache.Close()

_ = cache.Set("k1", []byte("v"))
if v, ok := cache.Get("k1"); ok { _ = v }

c := cache.ParameterC()
_ = c
```


## Acknowledgments and References

This library is a reimplementation based on the formulas and design principles of **AdaptSize**, a paper by Akamai researchers published at NSDI 2017. We acknowledge the prior work of the authors and their institutions. This is an independent third-party implementation and is not affiliated with the paper’s authors, their institutions, or the conference.

**References**
- AdaptSize, Proceedings of the 14th USENIX Symposium on Networked Systems Design and Implementation (NSDI), 2017. Akamai Technologies.  
  Core elements in this implementation: size-aware admission `exp(-size/c)`, a closed-form in-cache probability derived from the theorem, solving `μ` from the capacity constraint, and a global search that maximizes OHR (Object Hit Ratio).

**Notes**
- The API and parameter tuning implemented here follow the paper’s model; we adopt `exp(-size/c)` to match the paper’s notation.
- This code is not the paper’s official implementation. Algorithmic accuracy and applicability depend on the workload.
