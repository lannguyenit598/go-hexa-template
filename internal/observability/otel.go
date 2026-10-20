package observability

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Shutdown flushes buffered data and closes exporters.
type Shutdown func(context.Context) error

// Setup enables OpenTelemetry when standard environment variables configure OTLP
// (OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_SERVICE_NAME, OTEL_TRACES_SAMPLER...). Otherwise,
// global providers remain no-ops: instrumentation still runs with negligible overhead,
// and no exporter tries localhost:4318 and logs connection failures.
func Setup(ctx context.Context, serviceVersion string) (Shutdown, error) {
	// Always install the propagator so upstream trace context is forwarded even without export.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	if !Enabled(os.Getenv) {
		return func(context.Context) error { return nil }, nil
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			attribute.String("service.name", "userapi"),
			attribute.String("service.version", serviceVersion),
		),
		resource.WithFromEnv(), // OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES override the values above.
		resource.WithTelemetrySDK(),
		resource.WithHost(),
	)
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}

	traceExp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("otlp trace exporter: %w", err)
	}
	// Read sampler from OTEL_TRACES_SAMPLER / OTEL_TRACES_SAMPLER_ARG (default: parentbased_always_on).
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExp), sdktrace.WithResource(res))

	metricExp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		_ = tp.Shutdown(ctx)
		return nil, fmt.Errorf("otlp metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)), sdkmetric.WithResource(res))

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)

	return func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}, nil
}

// Enabled reports an OTLP endpoint is configured and OTEL_SDK_DISABLED is not set to disable it.
func Enabled(getenv func(string) string) bool {
	if strings.EqualFold(getenv("OTEL_SDK_DISABLED"), "true") {
		return false
	}
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"} {
		if getenv(k) != "" {
			return true
		}
	}
	return false
}
