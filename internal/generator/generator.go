// Package generator emits two families of synthetic metrics, each aimed at a
// specific kind of Coralogix recording rule.
//
// Test case 1: a high cardinality counter carrying a customer_id label, for
// testing a rule that aggregates the label away.
//
// Test case 2: SLO numerator and denominator counters, for testing rules that
// pre-aggregate and then divide.
package generator

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"os"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Config controls the size and cadence of the generated data.
type Config struct {
	// Prefix is prepended to every metric name.
	Prefix string
	// CustomerCount is the size of the customer_id label space.
	CustomerCount int
	// ActivePerTick is how many distinct customers are sampled per invocation.
	// Keeping this below CustomerCount means the active series set churns,
	// which is what makes high cardinality expensive in the first place.
	ActivePerTick int
	// TickSeconds is how much simulated wall clock each invocation represents.
	// Set it to the EventBridge schedule interval.
	TickSeconds int
}

// ConfigFromEnv reads generator tuning from the Lambda environment.
func ConfigFromEnv() Config {
	return Config{
		Prefix:        getenv("METRIC_PREFIX", "lab"),
		CustomerCount: getenvInt("CUSTOMER_COUNT", 2000),
		ActivePerTick: getenvInt("ACTIVE_CUSTOMERS_PER_TICK", 400),
		TickSeconds:   getenvInt("TICK_SECONDS", 60),
	}
}

type serviceSpec struct {
	name        string
	routes      []string
	baseRPS     float64
	baseFailure float64
}

var services = []serviceSpec{
	{name: "checkout-api", routes: []string{"/v1/cart", "/v1/checkout", "/v1/payment", "/v1/receipt"}, baseRPS: 120, baseFailure: 0.004},
	{name: "search-api", routes: []string{"/v1/search", "/v1/suggest", "/v1/facets"}, baseRPS: 340, baseFailure: 0.002},
	{name: "media-api", routes: []string{"/v1/upload", "/v1/thumbnail", "/v1/stream"}, baseRPS: 65, baseFailure: 0.011},
}

var regions = []string{"eu-central-1", "us-east-1"}

var tiers = []string{"free", "pro", "enterprise"}

// customer is one member of the high cardinality label space. Its service,
// route and tier are fixed at catalog build time so a given customer_id always
// produces the same series, no matter which Lambda container emits it.
type customer struct {
	id      string
	svc     int
	route   int
	tier    string
	baseRPS float64
}

// Generator holds the instruments and the customer catalog. Build it once per
// execution environment: the OTel SDK keeps cumulative counter state per
// attribute set, so recreating it would reset every series.
type Generator struct {
	cfg       Config
	customers []customer
	zipf      *rand.Zipf
	zipfSrc   *rand.Rand

	apiRequests metric.Int64Counter
	sloGood     metric.Int64Counter
	sloTotal    metric.Int64Counter
	ticks       metric.Int64Counter
}

// New builds the instruments and the customer catalog.
func New(mp metric.MeterProvider, cfg Config) (*Generator, error) {
	if cfg.CustomerCount < 1 {
		cfg.CustomerCount = 1
	}
	if cfg.ActivePerTick < 1 || cfg.ActivePerTick > cfg.CustomerCount {
		cfg.ActivePerTick = cfg.CustomerCount
	}
	if cfg.TickSeconds < 1 {
		cfg.TickSeconds = 60
	}

	meter := mp.Meter("cx-metrics-gen")
	g := &Generator{cfg: cfg}
	var err error

	// Instrument names carry no _total suffix and no unit, on purpose.
	// Coralogix applies Prometheus normalisation on ingest, which appends
	// _total to a monotonic sum and folds the unit into the name. Naming this
	// "lab_api_requests_total" with unit "{request}" produced the actual
	// series "lab_api_requests_total__request__total". Leaving both off yields
	// the clean "lab_api_requests_total" that the recording rules expect.

	// Test case 1: the expensive one.
	g.apiRequests, err = meter.Int64Counter(
		cfg.Prefix+"_api_requests",
		metric.WithDescription("Synthetic API requests, labelled per customer. High cardinality on purpose."),
	)
	if err != nil {
		return nil, fmt.Errorf("create api requests counter: %w", err)
	}

	// Test case 2: SLO numerator and denominator.
	g.sloGood, err = meter.Int64Counter(
		cfg.Prefix+"_slo_good_events",
		metric.WithDescription("Synthetic SLO numerator: requests that met the objective."),
	)
	if err != nil {
		return nil, fmt.Errorf("create slo good counter: %w", err)
	}
	g.sloTotal, err = meter.Int64Counter(
		cfg.Prefix+"_slo_total_events",
		metric.WithDescription("Synthetic SLO denominator: all valid requests."),
	)
	if err != nil {
		return nil, fmt.Errorf("create slo total counter: %w", err)
	}

	// A single low cardinality heartbeat, so you can tell "the rule produced
	// nothing" apart from "the Lambda never ran".
	g.ticks, err = meter.Int64Counter(
		cfg.Prefix+"_metricsgen_ticks",
		metric.WithDescription("Number of generator invocations."),
	)
	if err != nil {
		return nil, fmt.Errorf("create ticks counter: %w", err)
	}

	g.buildCatalog()
	return g, nil
}

// buildCatalog generates the customer label space from a fixed seed, so every
// cold start produces the identical set of customer_id values.
func (g *Generator) buildCatalog() {
	rnd := rand.New(rand.NewSource(20260921))
	g.customers = make([]customer, 0, g.cfg.CustomerCount)
	for i := 0; i < g.cfg.CustomerCount; i++ {
		s := i % len(services)
		g.customers = append(g.customers, customer{
			id:      fmt.Sprintf("cust-%05d", i),
			svc:     s,
			route:   rnd.Intn(len(services[s].routes)),
			tier:    tiers[weightedTier(rnd)],
			baseRPS: 0.05 + rnd.Float64()*1.2,
		})
	}

	// Zipf sampling: a handful of customers are very chatty and the long tail
	// shows up rarely, which is what real per-tenant traffic looks like.
	g.zipfSrc = rand.New(rand.NewSource(time.Now().UnixNano()))
	g.zipf = rand.NewZipf(g.zipfSrc, 1.25, 1.0, uint64(g.cfg.CustomerCount-1))
}

func weightedTier(rnd *rand.Rand) int {
	switch f := rnd.Float64(); {
	case f < 0.70:
		return 0
	case f < 0.93:
		return 1
	default:
		return 2
	}
}

// Result is a small summary for the invocation log.
type Result struct {
	ActiveCustomers int
	APIEvents       int64
	SLOEvents       int64
	SeriesTouched   int
}

func (r Result) String() string {
	return fmt.Sprintf("customers=%d api_events=%d slo_events=%d series_touched=%d",
		r.ActiveCustomers, r.APIEvents, r.SLOEvents, r.SeriesTouched)
}

// Tick simulates one scheduling interval worth of traffic and records it into
// the counters. It does not export: the caller flushes.
func (g *Generator) Tick(ctx context.Context, now time.Time) Result {
	rnd := rand.New(rand.NewSource(now.UnixNano() ^ int64(os.Getpid())))
	window := float64(g.cfg.TickSeconds)
	global := diurnal(now) * weekly(now) * burst(rnd)

	var res Result
	g.ticks.Add(ctx, 1)
	res.SeriesTouched++

	res.ActiveCustomers, res.APIEvents, res.SeriesTouched = g.tickHighCardinality(ctx, rnd, now, window, global, res.SeriesTouched)
	sloEvents, sloSeries := g.tickSLO(ctx, rnd, now, window, global)
	res.SLOEvents = sloEvents
	res.SeriesTouched += sloSeries

	return res
}

// tickHighCardinality drives test case 1.
func (g *Generator) tickHighCardinality(ctx context.Context, rnd *rand.Rand, now time.Time, window, global float64, series int) (int, int64, int) {
	active := make(map[int]struct{}, g.cfg.ActivePerTick)
	// Oversample, because Zipf repeats the head heavily and we dedupe.
	for attempts := 0; len(active) < g.cfg.ActivePerTick && attempts < g.cfg.ActivePerTick*12; attempts++ {
		active[int(g.zipf.Uint64())] = struct{}{}
	}

	var events int64
	for idx := range active {
		c := g.customers[idx]
		svc := services[c.svc]
		route := svc.routes[c.route]

		total := poisson(rnd, c.baseRPS*window*global*jitter(rnd, 0.12))
		if total <= 0 {
			continue
		}

		failRate := svc.baseFailure * degradation(svc.name, now) * jitter(rnd, 0.25)
		ok, clientErr, serverErr := splitStatus(rnd, total, failRate)

		base := []attribute.KeyValue{
			attribute.String("customer_id", c.id),
			attribute.String("service", svc.name),
			attribute.String("route", route),
			attribute.String("tier", c.tier),
		}
		for _, sc := range []struct {
			class string
			n     int64
		}{{"2xx", ok}, {"4xx", clientErr}, {"5xx", serverErr}} {
			if sc.n <= 0 {
				continue
			}
			attrs := append(append([]attribute.KeyValue{}, base...), attribute.String("status_class", sc.class))
			g.apiRequests.Add(ctx, sc.n, metric.WithAttributes(attrs...))
			events += sc.n
			series++
		}
	}
	return len(active), events, series
}

// tickSLO drives test case 2. Deliberately low cardinality: the interesting
// part is the ratio, not the label explosion.
func (g *Generator) tickSLO(ctx context.Context, rnd *rand.Rand, now time.Time, window, global float64) (int64, int) {
	var events int64
	series := 0

	for _, svc := range services {
		degraded := degradation(svc.name, now)
		perRoute := svc.baseRPS / float64(len(svc.routes))

		for _, route := range svc.routes {
			for i, region := range regions {
				// Split traffic unevenly across regions so the per region
				// series are not carbon copies of each other.
				share := 0.65
				if i > 0 {
					share = 0.35
				}

				total := poisson(rnd, perRoute*window*global*share*jitter(rnd, 0.10))
				if total <= 0 {
					continue
				}

				// Latency violations plus hard errors both count as bad, so the
				// SLO ratio is a bit worse than the raw 5xx rate.
				failRate := clamp01(svc.baseFailure*degraded*jitter(rnd, 0.20) + 0.003*jitter(rnd, 0.35))
				bad := poisson(rnd, float64(total)*failRate)
				if bad > total {
					bad = total
				}
				good := total - bad

				attrs := metric.WithAttributes(
					attribute.String("service", svc.name),
					attribute.String("route", route),
					attribute.String("region", region),
				)
				g.sloTotal.Add(ctx, total, attrs)
				g.sloGood.Add(ctx, good, attrs)

				events += total
				series += 2
			}
		}
	}
	return events, series
}

// ExpectedPeakSeries is a rough upper bound on the customer_id series count,
// handy for sanity checking what the recording rule is supposed to collapse.
func (g *Generator) ExpectedPeakSeries() int {
	return int(math.Round(float64(g.cfg.CustomerCount) * 2.2))
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
