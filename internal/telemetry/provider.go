// Package telemetry wires up an OTLP/gRPC metric pipeline pointed at Coralogix.
//
// This runs as a long lived process, so the pipeline is the ordinary one: a
// periodic reader exports on a fixed interval in the background, and Shutdown
// flushes whatever is left when the pod is terminating.
package telemetry

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	"google.golang.org/grpc/credentials"
)

// Config holds everything needed to reach a Coralogix OTLP ingress endpoint.
type Config struct {
	// Endpoint is host:port, for example ingress.eu2.coralogix.com:443.
	Endpoint string
	// APIKey is a Coralogix "Send Your Data" API key.
	APIKey string
	// Application and Subsystem are the Coralogix routing labels.
	Application string
	Subsystem   string
	// ServiceName lands on the metric as a resource attribute.
	ServiceName string
	// InstanceID distinguishes replicas. In Kubernetes this is the pod name,
	// injected through the downward API. Empty means the attribute is omitted,
	// which is what you want with a single replica.
	InstanceID string
	// Insecure disables TLS. Only useful when pointing at a local collector.
	Insecure bool
	// ExportInterval is how often the periodic reader pushes to Coralogix.
	ExportInterval time.Duration
	// ExportTimeout bounds a single OTLP push.
	ExportTimeout time.Duration
}

// ConfigFromEnv reads the configuration from the container environment.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		Endpoint:       getenv("CORALOGIX_ENDPOINT", "ingress.eu2.coralogix.com:443"),
		APIKey:         os.Getenv("CORALOGIX_API_KEY"),
		Application:    getenv("CORALOGIX_APPLICATION", "lab"),
		Subsystem:      getenv("CORALOGIX_SUBSYSTEM", "metrics-gen"),
		ServiceName:    getenv("OTEL_SERVICE_NAME", "cx-metrics-gen"),
		InstanceID:     os.Getenv("INSTANCE_ID"),
		Insecure:       strings.EqualFold(os.Getenv("CORALOGIX_INSECURE"), "true"),
		ExportInterval: time.Duration(getenvInt("EXPORT_INTERVAL_SECONDS", 60)) * time.Second,
		ExportTimeout:  20 * time.Second,
	}

	if cfg.APIKey == "" {
		return cfg, errors.New("CORALOGIX_API_KEY is not set")
	}
	// Strip a scheme if someone pasted a full URL: the gRPC exporter wants host:port.
	cfg.Endpoint = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(cfg.Endpoint, "https://"), "http://"), "/")
	if !strings.Contains(cfg.Endpoint, ":") {
		cfg.Endpoint += ":443"
	}
	if cfg.ExportInterval < time.Second {
		cfg.ExportInterval = time.Second
	}
	if cfg.ExportTimeout > cfg.ExportInterval {
		cfg.ExportTimeout = cfg.ExportInterval
	}
	return cfg, nil
}

// NewMeterProvider builds the MeterProvider. Cumulative temporality is forced
// because the whole point of the lab is to exercise PromQL rate() and
// recording rules downstream.
func NewMeterProvider(ctx context.Context, cfg Config) (*sdkmetric.MeterProvider, error) {
	opts := []otlpmetricgrpc.Option{
		otlpmetricgrpc.WithEndpoint(cfg.Endpoint),
		otlpmetricgrpc.WithHeaders(map[string]string{
			"Authorization": "Bearer " + cfg.APIKey,
		}),
		otlpmetricgrpc.WithTimeout(cfg.ExportTimeout),
		otlpmetricgrpc.WithTemporalitySelector(func(sdkmetric.InstrumentKind) metricdata.Temporality {
			return metricdata.CumulativeTemporality
		}),
	}
	if cfg.Insecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	} else {
		opts = append(opts, otlpmetricgrpc.WithTLSCredentials(
			credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12}),
		))
	}

	exporter, err := otlpmetricgrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("create otlp metric exporter: %w", err)
	}

	// Coralogix routes metrics using these resource attributes.
	attrs := []attribute.KeyValue{
		attribute.String("service.name", cfg.ServiceName),
		attribute.String("cx.application.name", cfg.Application),
		attribute.String("cx.subsystem.name", cfg.Subsystem),
	}
	if cfg.InstanceID != "" {
		attrs = append(attrs, attribute.String("service.instance.id", cfg.InstanceID))
	}
	res := resource.NewWithAttributes("", attrs...)

	reader := sdkmetric.NewPeriodicReader(exporter,
		sdkmetric.WithInterval(cfg.ExportInterval),
		sdkmetric.WithTimeout(cfg.ExportTimeout),
	)

	return sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(reader),
	), nil
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
