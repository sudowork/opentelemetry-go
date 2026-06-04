// Package regression contains a self-contained benchmark demonstrating the
// per-Collect memory regression introduced by the histogram aggregation
// rewrite in go.opentelemetry.io/otel/sdk/metric v1.40.0 (#7474), part of
// the broader optimization tracked at #7796.
//
// The cumulative-histogram collect path allocates a fresh []uint64 for
// every series' BucketCounts on every Collect cycle, because it passes a
// fresh local variable into loadCountsInto rather than the destination
// slot's existing slice (which is what the delta path does). At high
// cardinality this dominates allocation. See README.md for source
// references.
//
// To compare versions, change the otel/sdk/metric and otel/metric require
// lines in go.mod and re-run.
package regression_test

import (
	"context"
	"fmt"
	"runtime"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// cardinalities span a realistic range. Production services in the wild have
// 100k-1M+ unique series for a single histogram (one bucket counter per
// unique attribute-set).
var cardinalities = []int{1_000, 10_000, 100_000}

func makeAttrs(n int) []attribute.Set {
	attrs := make([]attribute.Set, n)
	for i := 0; i < n; i++ {
		attrs[i] = attribute.NewSet(
			attribute.String("series_id", fmt.Sprintf("series_%07d", i)),
			attribute.String("region", "us-east-1"),
			attribute.String("service", "voice"),
		)
	}
	return attrs
}

func newProvider() (*sdkmetric.MeterProvider, *sdkmetric.ManualReader) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	return provider, reader
}

// BenchmarkHistogramCollectOnly isolates Collect()'s cost. After priming
// every series with one measurement, the loop only calls Collect.
// This is the cleanest demonstration of the hot/cold swap allocation cost
// (cumulative temporality still emits every active series each Collect).
func BenchmarkHistogramCollectOnly(b *testing.B) {
	for _, n := range cardinalities {
		b.Run(fmt.Sprintf("series=%d", n), func(b *testing.B) {
			ctx := context.Background()
			provider, reader := newProvider()
			defer func() { _ = provider.Shutdown(ctx) }()

			h, err := provider.Meter("benchmark").Float64Histogram("test_histogram")
			if err != nil {
				b.Fatal(err)
			}
			attrs := makeAttrs(n)
			for _, a := range attrs {
				h.Record(ctx, 1.0, metric.WithAttributeSet(a))
			}

			rm := metricdata.ResourceMetrics{}
			runtime.GC()
			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				if err := reader.Collect(ctx, &rm); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()

			b.ReportMetric(float64(n), "series")
		})
	}
}

// BenchmarkHistogramSteadyState models a production cycle: every series
// records once per cycle, then Collect() runs. This is what a
// PeriodicReader on a 20s interval drives.
func BenchmarkHistogramSteadyState(b *testing.B) {
	for _, n := range cardinalities {
		b.Run(fmt.Sprintf("series=%d", n), func(b *testing.B) {
			ctx := context.Background()
			provider, reader := newProvider()
			defer func() { _ = provider.Shutdown(ctx) }()

			h, err := provider.Meter("benchmark").Float64Histogram("test_histogram")
			if err != nil {
				b.Fatal(err)
			}
			attrs := makeAttrs(n)

			for _, a := range attrs {
				h.Record(ctx, 1.0, metric.WithAttributeSet(a))
			}
			rm := metricdata.ResourceMetrics{}
			if err := reader.Collect(ctx, &rm); err != nil {
				b.Fatal(err)
			}

			runtime.GC()
			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				for _, a := range attrs {
					h.Record(ctx, float64(i), metric.WithAttributeSet(a))
				}
				if err := reader.Collect(ctx, &rm); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()

			b.ReportMetric(float64(n), "series")
		})
	}
}

// BenchmarkSumCollectOnly is the direct analogue for sums — the case
// PR #7427 itself benchmarks.
func BenchmarkSumCollectOnly(b *testing.B) {
	for _, n := range cardinalities {
		b.Run(fmt.Sprintf("series=%d", n), func(b *testing.B) {
			ctx := context.Background()
			provider, reader := newProvider()
			defer func() { _ = provider.Shutdown(ctx) }()

			c, err := provider.Meter("benchmark").Int64Counter("test_counter")
			if err != nil {
				b.Fatal(err)
			}
			attrs := makeAttrs(n)
			for _, a := range attrs {
				c.Add(ctx, 1, metric.WithAttributeSet(a))
			}

			rm := metricdata.ResourceMetrics{}
			runtime.GC()
			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				if err := reader.Collect(ctx, &rm); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()

			b.ReportMetric(float64(n), "series")
		})
	}
}

// BenchmarkSumSteadyState — sum analogue of HistogramSteadyState.
func BenchmarkSumSteadyState(b *testing.B) {
	for _, n := range cardinalities {
		b.Run(fmt.Sprintf("series=%d", n), func(b *testing.B) {
			ctx := context.Background()
			provider, reader := newProvider()
			defer func() { _ = provider.Shutdown(ctx) }()

			c, err := provider.Meter("benchmark").Int64Counter("test_counter")
			if err != nil {
				b.Fatal(err)
			}
			attrs := makeAttrs(n)

			for _, a := range attrs {
				c.Add(ctx, 1, metric.WithAttributeSet(a))
			}
			rm := metricdata.ResourceMetrics{}
			if err := reader.Collect(ctx, &rm); err != nil {
				b.Fatal(err)
			}

			runtime.GC()
			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				for _, a := range attrs {
					c.Add(ctx, 1, metric.WithAttributeSet(a))
				}
				if err := reader.Collect(ctx, &rm); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()

			b.ReportMetric(float64(n), "series")
		})
	}
}

// BenchmarkHistogramRecord measures the Record() hot path with many series
// (low lock contention on any given series). Each goroutine round-robins
// across 10k attribute sets, so #7474's sync.Map+atomics architecture has
// little to do.
func BenchmarkHistogramRecord(b *testing.B) {
	ctx := context.Background()
	provider, _ := newProvider()
	defer func() { _ = provider.Shutdown(ctx) }()

	h, err := provider.Meter("benchmark").Float64Histogram("test_histogram")
	if err != nil {
		b.Fatal(err)
	}
	const n = 10_000
	attrs := makeAttrs(n)
	for _, a := range attrs {
		h.Record(ctx, 1.0, metric.WithAttributeSet(a))
	}

	runtime.GC()
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			h.Record(ctx, 1.0, metric.WithAttributeSet(attrs[i%n]))
			i++
		}
	})
}

// BenchmarkHistogramRecordContended is the contention-heavy variant: every
// goroutine hits the same single attribute set, so the v1.39 per-instrument
// mutex serializes them. This is the workload #7474 was optimizing for.
func BenchmarkHistogramRecordContended(b *testing.B) {
	ctx := context.Background()
	provider, _ := newProvider()
	defer func() { _ = provider.Shutdown(ctx) }()

	h, err := provider.Meter("benchmark").Float64Histogram("test_histogram")
	if err != nil {
		b.Fatal(err)
	}
	hot := attribute.NewSet(attribute.String("series_id", "hot"))
	h.Record(ctx, 1.0, metric.WithAttributeSet(hot))

	runtime.GC()
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			h.Record(ctx, 1.0, metric.WithAttributeSet(hot))
		}
	})
}

// TestHeapGrowth runs many Collect cycles at production-scale cardinality
// and logs heap occupancy. It is what benchmarks alone cannot show: in
// production the regression manifests as monotonically growing heap and
// GC pressure across many 20s PeriodicReader cycles, not a single hot
// allocation. Use -v to see the log lines.
//
// To run only this: go test -run TestHeapGrowth -v -timeout 30m
func TestHeapGrowth(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping heap growth test in -short mode")
	}

	const (
		numSeries = 100_000
		numCycles = 200
	)

	ctx := context.Background()
	provider, reader := newProvider()
	defer func() { _ = provider.Shutdown(ctx) }()

	h, err := provider.Meter("benchmark").Float64Histogram("test_histogram")
	if err != nil {
		t.Fatal(err)
	}
	attrs := makeAttrs(numSeries)

	rm := metricdata.ResourceMetrics{}

	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)
	t.Logf("baseline heap_inuse=%dMB num_gc=%d", baseline.HeapInuse/1024/1024, baseline.NumGC)

	for cycle := 0; cycle < numCycles; cycle++ {
		for _, a := range attrs {
			h.Record(ctx, float64(cycle), metric.WithAttributeSet(a))
		}
		if err := reader.Collect(ctx, &rm); err != nil {
			t.Fatal(err)
		}

		if cycle%20 == 0 || cycle == numCycles-1 {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			t.Logf("cycle=%-3d heap_inuse=%dMB heap_alloc=%dMB total_alloc=%dMB num_gc=%d pause_total_ms=%d",
				cycle,
				m.HeapInuse/1024/1024,
				m.HeapAlloc/1024/1024,
				m.TotalAlloc/1024/1024,
				m.NumGC-baseline.NumGC,
				(m.PauseTotalNs-baseline.PauseTotalNs)/1_000_000,
			)
		}
	}

	runtime.GC()
	var final runtime.MemStats
	runtime.ReadMemStats(&final)
	t.Logf("=== final after forced GC ===")
	t.Logf("heap_inuse=%dMB heap_alloc=%dMB total_alloc=%dMB num_gc=%d allocs_per_cycle=%dMB",
		final.HeapInuse/1024/1024,
		final.HeapAlloc/1024/1024,
		final.TotalAlloc/1024/1024,
		final.NumGC-baseline.NumGC,
		(final.TotalAlloc-baseline.TotalAlloc)/uint64(numCycles)/1024/1024,
	)
}
