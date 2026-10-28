package server

import (
	"context"
	"errors"
	"fmt"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"go.opentelemetry.io/otel/attribute"
	"log/slog"
	"time"

	kratosmetrics "github.com/go-kratos/kratos/contrib/otel/v3/metrics"
	kratostracing "github.com/go-kratos/kratos/contrib/otel/v3/tracing"
	"github.com/go-kratos/kratos/v3/middleware"
	"github.com/go-kratos/kratos/v3/middleware/metadata"
	"github.com/go-kratos/kratos/v3/middleware/recovery"
	"github.com/go-kratos/kratos/v3/middleware/validate"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	otelprometheus "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	metricSdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	traceSdk "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

type Observability struct {
	registry          *prometheus.Registry
	meterProvider     *metricSdk.MeterProvider
	tracerProvider    *traceSdk.TracerProvider
	requests          metric.Int64Counter
	seconds           metric.Float64Histogram
	workerAttempts    metric.Int64Counter
	evidenceAge       metric.Float64Gauge
	providerReachable metric.Int64Gauge
}

func NewObservability(name, version string, readiness *Readiness) (*Observability, error) {
	if err := kratosmetrics.EnableOTELExemplar(); err != nil {
		return nil, fmt.Errorf("enable OpenTelemetry exemplars: %w", err)
	}

	registry := prometheus.NewRegistry()
	if err := registry.Register(prometheus.NewGoCollector()); err != nil {
		return nil, fmt.Errorf("register Go collector: %w", err)
	}
	if err := registry.Register(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{})); err != nil {
		return nil, fmt.Errorf("register process collector: %w", err)
	}
	exporter, err := otelprometheus.New(
		otelprometheus.WithRegisterer(registry),
		otelprometheus.WithoutScopeInfo(),
	)
	if err != nil {
		return nil, fmt.Errorf("create OpenTelemetry Prometheus exporter: %w", err)
	}

	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(name),
		semconv.ServiceVersion(version),
	)
	meterProvider := metricSdk.NewMeterProvider(
		metricSdk.WithResource(res),
		metricSdk.WithReader(exporter),
		metricSdk.WithView(kratosmetrics.DefaultSecondsHistogramView(kratosmetrics.DefaultServerSecondsHistogramName)),
	)
	tracerProvider := traceSdk.NewTracerProvider(
		traceSdk.WithResource(res),
		traceSdk.WithSampler(traceSdk.ParentBased(traceSdk.AlwaysSample())),
	)

	meter := meterProvider.Meter(name)
	requests, err := kratosmetrics.DefaultRequestsCounter(meter, kratosmetrics.DefaultServerRequestsCounterName)
	if err != nil {
		return nil, fmt.Errorf("create Kratos server request counter: %w", err)
	}
	seconds, err := kratosmetrics.DefaultSecondsHistogram(meter, kratosmetrics.DefaultServerSecondsHistogramName)
	if err != nil {
		return nil, fmt.Errorf("create Kratos server latency histogram: %w", err)
	}
	if _, err := meter.Int64ObservableGauge(
		"ani_runtime_ready",
		metric.WithDescription("Whether lifecycle, worker and Network database readiness checks pass."),
		metric.WithInt64Callback(func(_ context.Context, observer metric.Int64Observer) error {
			if readiness.Ready() {
				observer.Observe(1)
			} else {
				observer.Observe(0)
			}
			return nil
		}),
	); err != nil {
		return nil, fmt.Errorf("create readiness gauge: %w", err)
	}
	otel.SetMeterProvider(meterProvider)
	otel.SetTracerProvider(tracerProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	workerAttempts, err := meter.Int64Counter("ani_network_worker_attempts", metric.WithDescription("Durable worker attempts by bounded result."))
	if err != nil {
		return nil, err
	}
	providerReachable, err := meter.Int64Gauge("ani_network_provider_reachable", metric.WithDescription("Whether the most recent provider observation was reachable; absence means not observed yet."))
	if err != nil {
		return nil, err
	}
	evidenceAge, err := meter.Float64Gauge("ani_network_applied_evidence_age_seconds")
	if err != nil {
		return nil, err
	}
	return &Observability{
		evidenceAge:    evidenceAge,
		registry:       registry,
		workerAttempts: workerAttempts, providerReachable: providerReachable,
		meterProvider:  meterProvider,
		tracerProvider: tracerProvider,
		requests:       requests,
		seconds:        seconds,
	}, nil
}

func (o *Observability) Gatherer() prometheus.Gatherer {
	return o.registry
}

func (o *Observability) ServerMiddleware(logger *slog.Logger) []middleware.Middleware {
	return []middleware.Middleware{
		recovery.Recovery(recovery.WithLogger(logger)),
		metadata.Server(),
		kratostracing.Server(kratostracing.WithTracerProvider(o.tracerProvider)),
		requestLogging(logger),
		kratosmetrics.Server(
			kratosmetrics.WithRequests(o.requests),
			kratosmetrics.WithSeconds(o.seconds),
		),
		validate.Validator(),
	}
}

func (o *Observability) Shutdown(ctx context.Context) error {
	return errors.Join(
		o.meterProvider.Shutdown(ctx),
		o.tracerProvider.Shutdown(ctx),
	)
}

func (o *Observability) ObserveWork(ctx context.Context, logger *slog.Logger, work biz.Work, progress biz.Progress, err error) {
	if progress.ProviderError != nil {
		cause := progress.ProviderError
		var failure *biz.ProviderError
		if errors.As(cause, &failure) && failure.Cause != nil {
			cause = failure.Cause
		}
		logger.WarnContext(ctx, "network provider request failed", "resource_type", work.Resource.Kind, "resource_id", work.Resource.ID, "operation_id", work.Operation.ID, "binding_id", work.BindingID, "epoch", work.Epoch, "reason", progress.Reason, "error", cause)
	}
	result := "observed"
	if work.ActiveOperation {
		result = string(progress.OperationState)
	}
	if errors.Is(err, biz.ErrLeaseLost) {
		result = "lease_lost"
	} else if err != nil {
		result = "database_error"
	}
	o.workerAttempts.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result), attribute.String("kind", work.Resource.Kind)))
	if err == nil && progress.Observed {
		o.evidenceAge.Record(ctx, time.Since(progress.Proof.CollectedAt).Seconds(), metric.WithAttributes(attribute.String("kind", work.Resource.Kind)))
	}
	if err == nil {
		reachable := int64(1)
		if progress.Reason == biz.ProviderUnavailable || progress.Reason == biz.ProviderUnknown {
			reachable = 0
		}
		o.providerReachable.Record(ctx, reachable)
	}
	if work.ActiveOperation || err != nil || work.Resource.State != progress.State || work.Resource.Reason != progress.Reason {
		logger.InfoContext(ctx, "network reconciliation", "tenant_id", work.Resource.TenantID, "resource_type", work.Resource.Kind, "resource_id", work.Resource.ID, "operation_id", work.Operation.ID, "epoch", work.Epoch, "resource_state", progress.State, "result", result, "reason", progress.Reason)
	}
}

func (o *Observability) ObserveAttachmentWork(ctx context.Context, work biz.AttachmentWork, p biz.AttachmentProgress, err error) {
	result := "observed"
	if errors.Is(err, biz.ErrLeaseLost) {
		result = "lease_lost"
	} else if err != nil {
		result = "database_error"
	}
	o.workerAttempts.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", "attachment"), attribute.String("result", result)))
	if err == nil && p.Observed {
		o.evidenceAge.Record(ctx, time.Since(p.Proof.CollectedAt).Seconds(), metric.WithAttributes(attribute.String("kind", "attachment")))
	}
}

func (o *Observability) ObserveVirtualSwitchWork(ctx context.Context, logger *slog.Logger, work biz.VirtualSwitchWork, progress biz.VirtualSwitchProgress, err error) {
	if progress.ProviderError != nil {
		cause := progress.ProviderError
		var failure *biz.ProviderError
		if errors.As(cause, &failure) && failure.Cause != nil {
			cause = failure.Cause
		}
		logger.WarnContext(ctx, "virtual switch provider request failed", "resource_type", "virtual_switch", "resource_id", work.Switch.ID, "operation_id", work.Switch.LastOperationID, "binding_id", work.Switch.BindingID, "epoch", work.Epoch, "action", progress.Action, "node_name", progress.NodeName, "node_uid", progress.NodeUID, "binding_state", progress.BindingState, "reason", progress.Reason, "error", cause)
	}
	result := progress.BindingState
	if progress.Observed {
		result = "observed"
	} else if progress.ProviderError != nil {
		result = "provider_error"
	}
	if errors.Is(err, biz.ErrLeaseLost) {
		result = "lease_lost"
	} else if err != nil {
		result = "database_error"
	}
	o.workerAttempts.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", "virtual_switch"), attribute.String("result", result)))
	if err == nil && progress.Observed && !progress.CollectedAt.IsZero() {
		o.evidenceAge.Record(ctx, time.Since(progress.CollectedAt).Seconds(), metric.WithAttributes(attribute.String("kind", "virtual_switch")))
	}
	if err != nil || progress.ProviderError != nil || work.Switch.BindingState != progress.BindingState || work.Switch.Reason != progress.Reason {
		logger.InfoContext(ctx, "virtual switch reconciliation", "resource_id", work.Switch.ID, "operation_id", work.Switch.LastOperationID, "binding_id", work.Switch.BindingID, "epoch", work.Epoch, "binding_state", progress.BindingState, "observed", progress.Observed, "result", result, "reason", progress.Reason)
	}
}
