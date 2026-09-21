package generator

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// collect runs n ticks one simulated minute apart and returns the resulting
// cumulative metrics, so we can assert on shape rather than just on compilation.
func collect(t *testing.T, cfg Config, ticks int) metricdata.ResourceMetrics {
	t.Helper()

	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	g, err := New(mp, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	base := time.Date(2026, 9, 21, 13, 0, 0, 0, time.UTC)
	for i := 0; i < ticks; i++ {
		g.Tick(ctx, base.Add(time.Duration(i)*time.Minute))
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return rm
}

func sums(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Sum[int64] {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			s, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want Sum[int64]", name, m.Data)
			}
			return s
		}
	}
	t.Fatalf("metric %q not found", name)
	return metricdata.Sum[int64]{}
}

func TestHighCardinalityLabelSpace(t *testing.T) {
	cfg := Config{Prefix: "lab", CustomerCount: 2000, ActivePerTick: 400, TickSeconds: 60}
	rm := collect(t, cfg, 5)

	s := sums(t, rm, "lab_api_requests_total")
	if !s.IsMonotonic {
		t.Error("api requests counter must be monotonic")
	}
	if s.Temporality != metricdata.CumulativeTemporality {
		t.Errorf("temporality = %v, want cumulative", s.Temporality)
	}

	customers := map[string]struct{}{}
	classes := map[string]struct{}{}
	for _, dp := range s.DataPoints {
		v, ok := dp.Attributes.Value("customer_id")
		if !ok {
			t.Fatal("data point missing customer_id")
		}
		customers[v.AsString()] = struct{}{}
		c, _ := dp.Attributes.Value("status_class")
		classes[c.AsString()] = struct{}{}
	}

	// The whole point of test case 1 is that this number is uncomfortable.
	if len(customers) < 500 {
		t.Errorf("distinct customer_id = %d, want at least 500 after 5 ticks", len(customers))
	}
	if len(s.DataPoints) < len(customers) {
		t.Errorf("series = %d, fewer than distinct customers = %d", len(s.DataPoints), len(customers))
	}
	if _, ok := classes["5xx"]; !ok {
		t.Error("no 5xx series generated, error rules would have nothing to match")
	}
	t.Logf("distinct customers=%d series=%d status_classes=%d", len(customers), len(s.DataPoints), len(classes))
}

func TestSLONumeratorNeverExceedsDenominator(t *testing.T) {
	cfg := Config{Prefix: "lab", CustomerCount: 100, ActivePerTick: 50, TickSeconds: 60}
	rm := collect(t, cfg, 10)

	good := sums(t, rm, "lab_slo_good_events_total")
	total := sums(t, rm, "lab_slo_total_events_total")

	// A nil encoder would render every attribute set as the empty string and
	// silently collapse all series into one bucket.
	enc := attribute.DefaultEncoder()
	key := func(dp metricdata.DataPoint[int64]) string {
		return dp.Attributes.Encoded(enc)
	}
	totals := map[string]int64{}
	for _, dp := range total.DataPoints {
		totals[key(dp)] = dp.Value
	}

	var sumGood, sumTotal int64
	for _, dp := range good.DataPoints {
		tv, ok := totals[key(dp)]
		if !ok {
			t.Fatalf("good series %q has no matching total series", key(dp))
		}
		if dp.Value > tv {
			t.Errorf("good (%d) > total (%d) for %s", dp.Value, tv, key(dp))
		}
		sumGood += dp.Value
		sumTotal += tv
	}

	if sumTotal == 0 {
		t.Fatal("no SLO events generated")
	}
	ratio := float64(sumGood) / float64(sumTotal)
	// Loose bounds: it should look like a real service, neither perfect nor dead.
	if ratio < 0.80 || ratio > 0.9995 {
		t.Errorf("aggregate success ratio = %.4f, want a plausible SLI between 0.80 and 0.9995", ratio)
	}
	t.Logf("slo series=%d aggregate ratio=%.4f", len(total.DataPoints), ratio)
}

// TestVariationIsVisible guards the "not flat" requirement: if every tick
// produced the same numbers, recording rules would be indistinguishable from
// the raw series and the lab would prove nothing.
func TestVariationIsVisible(t *testing.T) {
	cfg := Config{Prefix: "lab", CustomerCount: 200, ActivePerTick: 200, TickSeconds: 60}

	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	g, err := New(mp, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	base := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)

	var perTick []int64
	var prev int64
	for i := 0; i < 24; i++ {
		// One tick per hour, so the diurnal curve has room to move.
		g.Tick(ctx, base.Add(time.Duration(i)*time.Hour))

		var rm metricdata.ResourceMetrics
		if err := reader.Collect(ctx, &rm); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		var cum int64
		for _, dp := range sums(t, rm, "lab_slo_total_events_total").DataPoints {
			cum += dp.Value
		}
		perTick = append(perTick, cum-prev)
		prev = cum
	}

	var min, max int64 = perTick[0], perTick[0]
	for _, v := range perTick {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	if min == 0 {
		t.Fatal("a tick produced zero events")
	}
	if float64(max)/float64(min) < 1.5 {
		t.Errorf("peak/trough = %.2f, want at least 1.5x so the curve is visible", float64(max)/float64(min))
	}
	t.Logf("hourly volumes min=%d max=%d peak/trough=%.2fx", min, max, float64(max)/float64(min))
}

// TestDegradationIsDeterministic makes sure a brownout can be found again in
// the same time window, which matters when you are eyeballing a recording rule
// against a historical range.
func TestDegradationIsDeterministic(t *testing.T) {
	at := time.Date(2026, 9, 21, 10, 3, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if got, want := degradation("checkout-api", at), degradation("checkout-api", at); got != want {
			t.Fatalf("degradation not deterministic: %v vs %v", got, want)
		}
	}

	var sawBrownout, sawHealthy bool
	for m := 0; m < 60; m++ {
		switch degradation("checkout-api", at.Add(time.Duration(m)*time.Minute)) {
		case 1.0:
			sawHealthy = true
		default:
			sawBrownout = true
		}
	}
	if !sawBrownout || !sawHealthy {
		t.Errorf("over one hour: brownout=%v healthy=%v, want both", sawBrownout, sawHealthy)
	}
}
