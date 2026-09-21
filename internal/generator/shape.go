package generator

import (
	"math"
	"math/rand"
	"time"
)

// The functions in this file give the generated data a shape. Without them
// every series is a flat line and a recording rule looks identical to the raw
// metric, which defeats the point of the lab.

// diurnal returns a 0.45 to 1.0 multiplier, peaking at 14:00 UTC and
// bottoming out around 02:00 UTC.
func diurnal(t time.Time) float64 {
	u := t.UTC()
	h := float64(u.Hour()) + float64(u.Minute())/60.0
	return 0.45 + 0.55*(0.5+0.5*math.Cos((h-14.0)/24.0*2.0*math.Pi))
}

// weekly damps traffic at the weekend and bumps it on Monday.
func weekly(t time.Time) float64 {
	switch t.UTC().Weekday() {
	case time.Saturday:
		return 0.58
	case time.Sunday:
		return 0.64
	case time.Monday:
		return 1.09
	default:
		return 1.0
	}
}

// burst occasionally multiplies a whole tick, so rate() has something spiky to
// chew on and so recording rule evaluation windows visibly smooth it out.
func burst(rnd *rand.Rand) float64 {
	if rnd.Float64() < 0.04 {
		return 1.8 + rnd.Float64()*1.4
	}
	return 1.0
}

// jitter is multiplicative gaussian noise, clamped so it never goes negative.
func jitter(rnd *rand.Rand, sigma float64) float64 {
	f := 1.0 + rnd.NormFloat64()*sigma
	if f < 0.1 {
		return 0.1
	}
	return f
}

// degradation drives the SLO error budget. It is a deterministic function of
// wall clock time rather than a coin flip, so a brownout is reproducible: you
// can go back to a window in Coralogix and the same dip is there.
//
// Each service runs on its own period, so the services do not all fail at once
// and a "sum by (service)" recording rule shows genuinely different ratios.
func degradation(svc string, t time.Time) float64 {
	period := int64(53)
	var offset int64
	for _, c := range svc {
		offset += int64(c)
	}
	phase := ((t.UTC().Unix() / 60) + offset%period) % period
	switch {
	case phase < 7:
		return 28.0 // hard brownout, roughly 7 minutes
	case phase < 13:
		return 4.5 // recovery tail
	default:
		return 1.0
	}
}

// splitStatus turns a request count and a failure rate into a 2xx / 4xx / 5xx
// breakdown. The 4xx share is client noise and is largely independent of the
// server side failure rate.
func splitStatus(rnd *rand.Rand, total int64, failRate float64) (ok, clientErr, serverErr int64) {
	if total <= 0 {
		return 0, 0, 0
	}
	serverErr = poisson(rnd, float64(total)*clamp01(failRate))
	clientErr = poisson(rnd, float64(total)*(0.008+rnd.Float64()*0.01))
	if serverErr+clientErr > total {
		serverErr = total * serverErr / (serverErr + clientErr)
		clientErr = total - serverErr
	}
	return total - serverErr - clientErr, clientErr, serverErr
}

// poisson draws a count around lambda. Knuth for small lambda, gaussian
// approximation above that, which is plenty for a lab.
func poisson(rnd *rand.Rand, lambda float64) int64 {
	if lambda <= 0 {
		return 0
	}
	if lambda > 30 {
		v := int64(math.Round(lambda + rnd.NormFloat64()*math.Sqrt(lambda)))
		if v < 0 {
			return 0
		}
		return v
	}
	l := math.Exp(-lambda)
	k := int64(0)
	p := 1.0
	for {
		p *= rnd.Float64()
		if p <= l {
			return k
		}
		k++
		if k > 1000 {
			return k
		}
	}
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}
