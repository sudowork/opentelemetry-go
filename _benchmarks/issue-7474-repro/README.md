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
  for histogram and counter aggregations, plus uncontended/contended Record
  benchmarks and a heap-growth integration test.
- `go.mod` — pinned to v1.39.0 as the baseline; flip to v1.43.0 (or any v1.40+)
  to measure the regression. Add `replace` directives to point at a local
  checkout of opentelemetry-go with the patch applied to measure that.
- `cumulative-histogram-collect-fix.patch` — candidate fix against v1.43.0's
  `sdk/metric/internal/aggregate/histogram.go`. Passes the full upstream
  `sdk/metric` test suite including `-race`.
- `results-*.txt` — captured `go test -bench` and `TestHeapGrowth` output
  for all three configurations.

## How to reproduce

```bash
# 1. Baseline at v1.39 (last release before the rewrite)
go test -bench='Benchmark(Histogram|Sum)' -benchmem -benchtime=2x -count=3 -timeout=10m \
  -run=^$ > v1.39.txt
go test -bench='BenchmarkHistogramRecordContended' -benchmem -benchtime=500ms -count=5 \
  -run=^$ >> v1.39.txt
go test -run TestHeapGrowth -v -timeout=10m > v1.39.heap.txt

# 2. Switch to unpatched v1.43
go get go.opentelemetry.io/otel/sdk/metric@v1.43.0 \
       go.opentelemetry.io/otel/metric@v1.43.0 \
       go.opentelemetry.io/otel@v1.43.0
go mod tidy
# ... rerun the same go test commands, output to v1.43.txt / v1.43.heap.txt

# 3. Switch to patched v1.43
git clone --depth 1 --branch v1.43.0 https://github.com/open-telemetry/opentelemetry-go.git /tmp/otel-go
( cd /tmp/otel-go && git apply /path/to/cumulative-histogram-collect-fix.patch )
go mod edit \
  -replace=go.opentelemetry.io/otel/sdk/metric=/tmp/otel-go/sdk/metric \
  -replace=go.opentelemetry.io/otel=/tmp/otel-go \
  -replace=go.opentelemetry.io/otel/metric=/tmp/otel-go/metric \
  -replace=go.opentelemetry.io/otel/sdk=/tmp/otel-go/sdk \
  -replace=go.opentelemetry.io/otel/trace=/tmp/otel-go/trace
go mod tidy
# ... rerun the same go test commands, output to v1.43-patched.txt / v1.43-patched.heap.txt

# 4. Compare
go install golang.org/x/perf/cmd/benchstat@latest
benchstat v1.39.txt v1.43.txt v1.43-patched.txt
```

## What we observed

Run on Linux/amd64 (4 vCPU sandbox), Intel Xeon @ 2.80GHz, Go 1.25.11.
Three configurations: v1.39.0 baseline, unpatched v1.43.0, and v1.43.0 with
the candidate fix in `cumulative-histogram-collect-fix.patch` applied.

### Histograms — allocations per Collect cycle

```
                                   │  v1.39   │     v1.43      │   v1.43+patch
                                   │   B/op   │ B/op  vs v1.39 │ B/op  vs v1.39
HistogramCollectOnly/series=1000     1.07 MiB   1.93 MiB +80%    1.01 MiB  -6%
HistogramCollectOnly/series=10000   10.69 MiB  19.23 MiB +80%   10.08 MiB  -6%
HistogramCollectOnly/series=100000   107 MiB     192 MiB +80%    101 MiB   -6%
HistogramSteadyState/series=1000      165 KiB  1.91 MiB +1057%   39.5 KiB -76%
HistogramSteadyState/series=10000     1.6 MiB  18.7 MiB +1067%   392 KiB  -76%
HistogramSteadyState/series=100000   16.0 MiB   187 MiB +1067%   3.8 MiB  -76%
```

### Histograms — wall time per Collect cycle

```
                                   │  v1.39   │      v1.43        │   v1.43+patch
                                   │  sec/op  │ sec/op   vs v1.39 │ sec/op  vs v1.39
HistogramCollectOnly/series=100000   172 ms    447 ms    +160%     245 ms   +42%
HistogramSteadyState/series=100000   145 ms    584 ms    +302%     282 ms   +95%
```

The unpatched v1.43 takes a ~12x memory hit and a 2.6-4.0x wall-time hit at
100k series in the steady-state pattern. With the patch applied, allocation
drops below v1.39 (slot reuse means no per-cycle `BucketCounts` allocation),
while wall time is still ~95% higher than v1.39 — the patch closes the
allocation gap entirely but doesn't recover the per-series CPU cost of
`sync.Map.Range` + atomic loads vs. v1.39's mutex-guarded map iteration.

### Record() hot path — uncontended vs. contended

```
                              │  v1.39   │    v1.43    │  v1.43+patch
                              │  ns/op   │ns/op  delta │ ns/op  delta
HistogramRecord                 21.1 µs    24.1 µs +14%  24.4 µs +16%
HistogramRecordContended        307 ns     287 ns  -7%    251 ns -18%
```

`HistogramRecord` runs 4 goroutines round-robining across 10k distinct series
— low lock contention. `HistogramRecordContended` runs 4 goroutines all
hitting the _same_ attribute set — maximum contention on the per-instrument
lock. The contended case is the workload #7474 explicitly optimized for, and
it does show the expected speedup (~7% unpatched, ~18% patched, modest at 4
vCPU; the original PR benchmarked on many-core machines).

The patch keeps the contended-Record win and removes the steady-state Collect
regression.

### Counters

```
SumCollectOnly/series=100000   44.6 MiB → 44.6 MiB → 44.6 MiB   no change
SumSteadyState/series=100000   43.5 MiB → 43.5 MiB → 43.5 MiB   no change
```

Counters never regressed; the patch is a no-op there because
`metricdata.DataPoint[N]` doesn't carry a per-series slice analogous to
`BucketCounts`. The structural quirk (`reset(... 0, ...) + append(newPt)`)
exists in `cumulativeSum.collect` too but doesn't matter for sums because
there's nothing large to throw away.

### Heap growth (200 cycles × 100k histogram series)

| Metric                  | v1.39   | v1.43    | v1.43+patch |
| ----------------------- | ------- | -------- | ----------- |
| Total bytes allocated   | 3675 MB | 37666 MB | **1228 MB** |
| Allocations per cycle   | 18 MB   | 188 MB   | **6 MB**    |
| GC count                | 13      | 93       | **7**       |
| Cumulative GC pause     | 1 ms    | 15 ms    | **0 ms**    |
| Wall time               | 42 s    | 75 s     | 48 s        |
| Resident heap (post-GC) | 282 MB  | 266 MB   | 266 MB      |

The patched version allocates 3x less per cycle than v1.39 and triggers half
the GCs, because `BucketCounts` slices and `Exemplars` are now reused across
cycles — which v1.39 never did either.

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

## Candidate fix — `cumulative-histogram-collect-fix.patch`

The patch (included in this directory) mirrors what the delta path already does:

1. Pre-size `h.DataPoints` to length `n`, capacity `n` so the per-series
   destination slots exist and can be addressed by index.
2. Replace the `append(hDPts, newPt)` pattern with an indexed write, and thread
   `&hDPts[i].BucketCounts` (and `&hDPts[i].Exemplars`) into the calls that
   fill them — the same trick the delta path uses at line 199.
3. Concurrent growth (`s.values.Len()` racing with new inserts mid-Range) falls
   back to `append` for the extra slots, and the slice is trimmed to the actual
   iteration count at the end so a shrunk `s.values` doesn't leak stale points.
4. Slots may be reused across cycles by different series (`sync.Map` iteration
   order isn't stable), so `Sum`/`Min`/`Max` are explicitly cleared when the
   instrument is configured with `noSum` / `noMinMax` or the new series has no
   recorded extrema yet, to avoid leaking values from the previous occupant.

Total diff: ~70 lines in one file. Passes the full upstream `sdk/metric`
test suite, including `-race`. Numbers above show it eliminates the
allocation regression entirely and recovers about half the wall-time
regression. The remaining wall-time gap appears to be the per-series cost
of `sync.Map.Range` + atomic loads, which would require a different
follow-up to address.

## License

This reproducer is released into the public domain — copy, paste, modify, and
upstream freely.
