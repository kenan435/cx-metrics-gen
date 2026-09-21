# cx-metrics-gen

A small Go service that pushes synthetic OTLP metrics to Coralogix, shaped
specifically so you can develop and test recording rules against data that
actually moves. Runs as a single replica Deployment on Kubernetes.

Two test cases:

| Test case | Metric | What it is for |
|---|---|---|
| 1. High cardinality | `lab_api_requests_total{customer_id, service, route, tier, status_class}` | A rule that aggregates `customer_id` away |
| 2. SLO numerator / denominator | `lab_slo_good_events_total`, `lab_slo_total_events_total` `{service, route, region}` | A rule that pre-aggregates then divides |

Plus `lab_metricsgen_ticks_total`, a single series heartbeat so you can tell
"the rule matched nothing" apart from "the pod is not running".

Metrics are **pushed** over OTLP/gRPC on a timer. There is no `/metrics`
endpoint and nothing scrapes this. The HTTP listener on `:8080` serves probes
and a `/status` page only.

## Layout

```
main.go                              Service loop, probes, graceful shutdown
internal/telemetry/provider.go       OTLP/gRPC exporter and MeterProvider
internal/generator/generator.go      Instruments, customer catalog, per tick logic
internal/generator/shape.go          Diurnal curve, bursts, brownouts, noise
internal/generator/generator_test.go Asserts the data shape, not just the build
Dockerfile                           Static binary on distroless
deploy/k8s/                          Namespace, ConfigMap, Service, Deployment, kustomization
recording-rules/                     Example rules for both test cases
.github/workflows/ci.yaml            Test, then build and push the image to GHCR
```

## Quick start

```bash
# 1. Namespace and API key
make secret CORALOGIX_API_KEY=cxtp_xxx

# 2. Point the ConfigMap at your Coralogix region if it is not EU2
$EDITOR deploy/k8s/configmap.yaml

# 3. Deploy
make deploy

# 4. Watch it tick
make logs
```

Expected log output:

```
starting: endpoint=ingress.eu2.coralogix.com:443 application=lab subsystem=metrics-gen instance="cx-metrics-gen-7d9f8b6c4-xk2p9" customers=2000 active_per_tick=400 tick=60s export_every=1m0s peak_series~4400
probe listener on :8080
tick 1: customers=400 api_events=12841 slo_events=31204 series_touched=1141
tick 2: customers=398 api_events=13102 slo_events=30877 series_touched=1138
```

Then give it ten minutes and check the rules in `recording-rules/`.

## Before the first deploy

Two things need to be true:

**1. The image has to exist.** CI builds and pushes
`ghcr.io/kenan435/cx-metrics-gen:main` on every push to `main`. Wait for the
`ci` workflow to go green before deploying, or build it yourself:

```bash
make image-push TAG=main
```

**2. The cluster has to be able to pull it.** GHCR packages inherit the
repository's visibility, so for a private repo the package is private too and
the pull will fail with `ImagePullBackOff`. Either flip the package to public
under **Packages > cx-metrics-gen > Package settings**, or create a pull
secret:

```bash
kubectl -n cx-metrics-gen create secret docker-registry ghcr \
  --docker-server=ghcr.io \
  --docker-username=kenan435 \
  --docker-password=<a PAT with read:packages>

kubectl -n cx-metrics-gen patch serviceaccount default \
  -p '{"imagePullSecrets":[{"name":"ghcr"}]}'
```

## Run it locally first

Quickest way to confirm the key and endpoint work before involving the cluster.
Same binary, same loop.

```bash
export CORALOGIX_API_KEY=cxtp_xxx
export CORALOGIX_ENDPOINT=ingress.eu2.coralogix.com:443
make run
```

## Configuration

All via environment variables. In the cluster these come from the ConfigMap,
except the API key, which comes from the Secret.

| Variable | Default | Notes |
|---|---|---|
| `CORALOGIX_API_KEY` | required | A "Send Your Data" API key. From the Secret |
| `CORALOGIX_ENDPOINT` | `ingress.eu2.coralogix.com:443` | `host:port`. A pasted `https://` URL is tolerated |
| `CORALOGIX_APPLICATION` | `lab` | Becomes the `cx.application.name` resource attribute |
| `CORALOGIX_SUBSYSTEM` | `metrics-gen` | Becomes `cx.subsystem.name` |
| `OTEL_SERVICE_NAME` | `cx-metrics-gen` | Becomes `service.name` |
| `INSTANCE_ID` | empty | Becomes `service.instance.id`. Set from the pod name via the downward API |
| `CORALOGIX_INSECURE` | `false` | Set `true` only to point at a local collector |
| `METRIC_PREFIX` | `lab` | Prepended to every metric name |
| `CUSTOMER_COUNT` | `2000` | Size of the `customer_id` label space |
| `ACTIVE_CUSTOMERS_PER_TICK` | `400` | Distinct customers sampled per tick |
| `TICK_SECONDS` | `60` | Simulated wall clock per tick, and the real tick interval |
| `EXPORT_INTERVAL_SECONDS` | `60` | How often the reader pushes to Coralogix |
| `HTTP_ADDR` | `:8080` | Probe listener |

Match the endpoint to your Coralogix region: `ingress.eu2.coralogix.com:443`
(EU2), `ingress.coralogix.com:443` (EU1), `ingress.cx498.coralogix.com:443`
(AP2), `ingress.coralogix.us:443` (US1), and so on.

## Data shape

Nothing is flat. Volume is a product of:

- a diurnal curve peaking at 14:00 UTC and bottoming out around 02:00 (0.45x to 1.0x),
- a weekly factor (weekends 0.58x to 0.64x, Monday 1.09x),
- a 4% chance per tick of a 1.8x to 3.2x burst,
- multiplicative gaussian noise per series,
- Poisson draws on every count, so no two ticks are identical.

Error rates are driven by `degradation()`, a deterministic function of wall
clock time: each service enters a roughly 7 minute brownout (28x its baseline
failure rate) followed by a 6 minute recovery tail, on its own 53 minute cycle.
Deterministic matters here, because you can go back to a window in Coralogix
and the same dip is still there while you iterate on a rule.

Customer traffic follows a Zipf distribution, so a handful of tenants are very
chatty and the long tail appears intermittently. The active customer set churns
between ticks, which is exactly what makes high cardinality expensive.

The customer catalog is built from a fixed seed, so a given `customer_id`
always maps to the same service, route and tier no matter which pod emits it.

## Recording rules

Both files under `recording-rules/` are plain Prometheus rule group YAML, ready
to paste into Coralogix under **Data Flow > Recording Rules**. Each file ends
with the PromQL to verify it.

The headline pair:

```promql
# before
count(count by (customer_id) (lab_api_requests_total))   # thousands
# after
count(lab:api_requests:rate5m)                           # tens
```

```promql
# SLO ratio, pre-aggregated then divided
lab:slo_success_ratio:rate5m
```

Give it about 10 minutes before judging anything: the rules use `[5m]` windows,
so they need at least two evaluation intervals of data underneath them.

## Things worth knowing

**Keep it at one replica.** Every replica builds the identical customer
catalog. They do not collide, because each pod stamps its own
`service.instance.id` from the pod name, but a second replica buys you twice
the series rather than more traffic per series. The Deployment uses the
`Recreate` strategy for the same reason: a rolling update would briefly run two
pods emitting the same metrics.

**Counters reset when the pod restarts,** and the restarted pod comes back with
a new `service.instance.id`. Both are normal Prometheus behaviour and `rate()`
handles them, but do not build anything on the raw counter value, and prefer
`sum by (...)` over `sum without (...)` in rules so a new instance label cannot
quietly regrow your cardinality.

**Cumulative temporality is forced.** It is set explicitly in
`internal/telemetry/provider.go` rather than left to the default, because
PromQL `rate()` downstream depends on it.

**Memory grows with cardinality.** Cumulative temporality means the SDK holds
one aggregation per attribute set for the life of the pod. At
`CUSTOMER_COUNT=2000` that is a few thousand series and the 512Mi limit is
generous. If you push `CUSTOMER_COUNT` into the tens of thousands, raise the
limit and watch for the OTel Go SDK's experimental cardinality limit
(`OTEL_GO_X_CARDINALITY_LIMIT`), which is off by default but will silently fold
overflow series into a single bucket if you turn it on.

**Metric naming.** The instruments are named with the `_total` suffix already
present, so Prometheus style normalisation on the Coralogix side does not
double-append it. Confirm the exact final names in Metrics Explorer before
writing rules against them.

**The pod is locked down** (non-root, read-only root filesystem, all
capabilities dropped, `RuntimeDefault` seccomp) because it costs nothing here.
It needs egress to your Coralogix ingress endpoint on 443 and nothing else.

## Tests

```bash
make test
```

The tests use a `ManualReader` and assert on the emitted data rather than on
compilation: that the `customer_id` space is genuinely large, that the SLO
numerator never exceeds its denominator per series, that the aggregate SLI is
plausible, that hourly volume varies by at least 1.5x, and that brownouts are
reproducible for a given timestamp.
