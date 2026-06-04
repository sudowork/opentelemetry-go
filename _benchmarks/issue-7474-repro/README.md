# sdk/metric cumulative-histogram per-Collect memory regression reproducer

A self-contained benchmark for the per-`Collect()` memory regression introduced
by the histogram aggregation rewrite in
`go.opentelemetry.io/otel/sdk/metric` v1.40.0:

- [#7474](https://github.com/open-telemetry/opentelemetry-go/pull/7474) — fixed-bucket
  histograms (the source of the regression measured here)
- [#7427](https://github.com/open-telemetry/opentelemetry-go/pull/7427) — sums
  (same architectural pattern; no measurable regression in this benchmark)
- [#7478](https://github.com/open-telemetry/opentelemetry-go/pull/7478) — gauges /
  lastvalue
- Tracker: [#7796](https://github.com/open-telemetry/opentelemetry-go/issues/7796)
  ("Optimize the metric SDK")

The Record() hot path is faster after the rewrite, but `cumulativeHistogram.collect()`
allocates a fresh `[]uint64` for every series' `BucketCounts` on every cycle,
because the cumulative path passes a freshly-declared local variable into
`loadCountsInto` rather than the destination slot's existing slice (which is
what the delta path correctly does). At high cardinality (>=100k series) this
dominates and drives runaway allocation rate, GC pressure, and OOMs in services
with short `PeriodicReader` intervals. See the
"Where the bytes are going" section below for line-level references.

## What's in here

- `regression_test.go` — `go test`-compatible benchmarks at 1k, 10k, 100k series
  for histogram and counter aggregations, plus a heap-growth integration test.
- `go.mod` — pinned to v1.39.0 as the baseline; flip to v1.43.0 (or any v1.40+)
  to measure the regression.

## How to reproduce

```bash
# 1. Baseline at v1.39 (last release before the rewrite)
go test -bench='Benchmark(Histogram|Sum)' -benchmem -benchtime=2x -count=3 -timeout=10m \
  -run=^$ > v1.39.txt

# 2. Switch to v1.43 (latest as of 2026-06)
go get go.opentelemetry.io/otel/sdk/metric@v1.43.0 \
       go.opentelemetry.io/otel/metric@v1.43.0 \
       go.opentelemetry.io/otel@v1.43.0
go mod tidy

# 3. Re-run
go test -bench='Benchmark(Histogram|Sum)' -benchmem -benchtime=2x -count=3 -timeout=10m \
  -run=^$ > v1.43.txt

# 4. Compare
go install golang.org/x/perf/cmd/benchstat@latest
benchstat v1.39.txt v1.43.txt

# 5. Heap-growth integration test (logs allocation/GC stats across 200 cycles)
go test -run TestHeapGrowth -v -timeout=10m
```

## What we observed

Run on Linux/amd64, Intel Xeon @ 2.80GHz, Go 1.25.11.

### Histograms (where production OOMed)

```
                                   │  v1.39   │             v1.43             │
                                   │   B/op   │      B/op       vs base       │
HistogramCollectOnly/series=1000     1.07 MiB    1.93 MiB   ~80% more
HistogramCollectOnly/series=10000   10.69 MiB   19.23 MiB   ~80% more
HistogramCollectOnly/series=100000   107 MiB     192 MiB    ~80% more
HistogramSteadyState/series=1000     165 KiB    1.91 MiB    +1057%  (~12x)
HistogramSteadyState/series=10000    1.6 MiB    18.7 MiB    +1067%  (~12x)
HistogramSteadyState/series=100000   16 MiB     187 MiB     +1067%  (~12x)

                                   │  v1.39   │             v1.43             │
                                   │  sec/op  │     sec/op     vs base        │
HistogramCollectOnly/series=100000   207 ms     499 ms     +141%
HistogramSteadyState/series=100000   186 ms     578 ms     +210%
```

`CollectOnly` primes every series once, then loops on `Collect`. `SteadyState`
records one new value per series per cycle, then collects — what a 20s
`PeriodicReader` does in production. The steady-state delta is the headline:
**~12x more allocated memory per collect cycle on histograms** at every
cardinality we tested.

### Counters (the case PR #7427 directly targets)

```
SumCollectOnly/series=100000   44.6 MiB → 44.6 MiB   no change
SumSteadyState/series=100000   43.5 MiB → 43.5 MiB   no change
```

Counters do not regress in this benchmark. The sums variant of the rewrite
shipped without a measurable allocation cost; the histogram follow-up
(#7474) is where the regression bites.

### Heap growth (200 cycles × 100k histogram series)

| Metric                  | v1.39   | v1.43    | Delta              |
| ----------------------- | ------- | -------- | ------------------ |
| Total bytes allocated   | 3675 MB | 37666 MB | +925% (10.3x)      |
| Allocations per cycle   | 18 MB   | 188 MB   | +944% (10.4x)      |
| GC count                | 13      | 93       | +615% (7.2x)       |
| Cumulative GC pause     | 1 ms    | 15 ms    | +1400% (15x)       |
| Wall time               | 42 s    | 75 s     | +78%               |
| Resident heap (post-GC) | 282 MB  | 266 MB   | similar (expected) |

The post-GC resident heap is roughly equal — cumulative aggregation retains
the same series state on both versions. What changes catastrophically is the
**allocation rate**, which is what drives GC pressure and OOM in services
running `PeriodicReader` on a tight interval.

## Why this matters in production

A service with 1M unique histogram series and a 20s `PeriodicReader` interval
allocates roughly:

- on v1.39: ~10x the per-100k number = ~180 MB per cycle ≈ 9 MB/s of churn
- on v1.43: ~10x the per-100k number = ~1.9 GB per cycle ≈ 94 MB/s of churn

The 94 MB/s figure is what took us from a stable 14-18 GB heap on v1.39 to OOM
within an hour after a transitive bump to v1.40+. The Go runtime's allocator
keeps up fine, but the heap grows faster than GC can reclaim it under load,
and the OTLP export payload (one fresh slice per series per cycle) balloons
past collector receive limits.

## Where the bytes are going (v1.43.0 source)

The regression is concentrated in `cumulativeHistogram.collect()` at
`sdk/metric/internal/aggregate/histogram.go`. The delta and cumulative paths
handle `BucketCounts` reuse differently:

```go
// delta path — reuses the destination slot's BucketCounts (histogram.go:199)
count := val.loadCountsInto(&hDPts[i].BucketCounts)

// cumulative path — allocates fresh per series (histogram.go:357-358)
var bucketCounts []uint64
count := val.hotColdPoint[readIdx].loadCountsInto(&bucketCounts)
```

`loadCountsInto` calls `reset(*into, length, capacity)` (`aggregate.go:157-162`)
which reuses the underlying array if `cap(*into) >= capacity` and otherwise
allocates. The delta path passes a pointer to last cycle's destination slot, so
capacity is reused. The cumulative path passes a pointer to a freshly-declared
nil local, so the call reduces to `make([]uint64, len(bounds)+1)` per series
per collect.

This is compounded by the surrounding loop structure. The outer `h.DataPoints`
slice header is reused via `reset(sData.DataPoints, 0, n)` (`histogram.go:343`)
— the backing array's capacity carries over. But length is reset to 0, and the
loop then does `hDPts = append(hDPts, newPt)` (`histogram.go:385`) with `newPt`
constructed fresh at lines 359-368. The previous cycle's `BucketCounts` and
`Exemplars` slices sitting in `h.DataPoints[i]` are overwritten by the fresh
struct and become garbage on the next GC. Per cycle this costs roughly
`numSeries * (len(bounds)+1) * 8` bytes for `BucketCounts` alone, which matches
the ~187 MiB/cycle we measure at 100k series with default boundaries.

The SDK source already flags this at `histogram.go:45-46`:

> `// TODO (#3047): Making copies for counts incurs a large memory allocation footprint. Alternatives should be explored.`

Sums (`cumulativeSum.collect` at `sum.go:151-195`) use the same
`reset(... 0, ...) + append(newPt)` pattern, but `metricdata.DataPoint[N]`
carries only scalars plus a small `Exemplars` slice — there is no `[]uint64`
analogue of `BucketCounts`. The structural quirk is identical; the per-series
byte cost just isn't large enough to show up. This matches the 0% regression
on the sum benchmarks above.

## Suggested fix

Mirror what the delta path already does. Two adjustments to
`cumulativeHistogram.collect()`:

1. Pre-size `h.DataPoints` to length `n`, capacity `n` so the per-series
   destination slots exist and can be addressed by index.
2. Replace the `append(hDPts, newPt)` pattern with an indexed write, and thread
   `&hDPts[i].BucketCounts` (and `&hDPts[i].Exemplars`) into the calls that
   fill them — the same trick the delta path uses at line 199. The cumulative
   case has the extra wrinkle that `s.values.Len()` is read concurrently and
   the iteration count can drift, but that can be handled by capping the index
   inside `Range` and trimming `hDPts` to `i` at the end.

This is a much narrower change than reverting the rewrite, keeps the
Record-path wins, and addresses the TODO at line 45 of the same file.

## License

This reproducer is released into the public domain — copy, paste, modify, and
upstream freely.
