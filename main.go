package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	otelprometheus "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric/instrument"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"golang.org/x/exp/slog"
)

const (
	Name = "github.com/snyk/polaris-o11y-canary"
)

func main() {
	var freq time.Duration
	var val int64
	var httpAddr string
	flag.DurationVar(&freq, "frequency", 5*time.Second, "frequency to produce data (log, metric)")
	flag.Int64Var(&val, "metric.val", 1, "value to increase counters by")
	flag.StringVar(&httpAddr, "http.addr", ":8080", "http listen address")
	flag.Parse()

	// setup logger
	ctx := context.Background()
	lg := slog.New(slog.NewJSONHandler(os.Stdout, nil)).WithGroup("polaris-o11y-canary")

	// setup metrics
	otlpCtr, hoCtr, err := initOTLPExporter(ctx)
	if err != nil {
		lg.LogAttrs(ctx, slog.LevelError, "setup otel exporter",
			slog.String("error", err.Error()),
		)
		os.Exit(1)
	}
	promCtr, hpCtr, handler, err := initPromExporter(ctx)
	if err != nil {
		lg.LogAttrs(ctx, slog.LevelError, "setup prom exporter",
			slog.String("error", err.Error()),
		)
		os.Exit(1)
	}
	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if r.UserAgent() == "Prometheus/" {
			hoCtr.Add(ctx, 1)
			hpCtr.Add(ctx, 1)
		}
		handler.ServeHTTP(w, r)
	})

	// run o11y producer
	go runProducer(ctx, lg, otlpCtr, promCtr, freq, val)

	// run http server
	lg.LogAttrs(ctx, slog.LevelInfo, "starting server", slog.String("address", httpAddr))
	err = http.ListenAndServe(httpAddr, nil)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		lg.LogAttrs(ctx, slog.LevelError, "unexpected server shutdown",
			slog.String("error", err.Error()),
		)
		os.Exit(1)
	}
	lg.LogAttrs(ctx, slog.LevelInfo, "server exiting")
}

func runProducer(ctx context.Context, lg *slog.Logger, otlp, prom instrument.Int64Counter, freq time.Duration, val int64) {
	tick := time.NewTicker(freq)
	lg = lg.WithGroup("canary")
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			otlp.Add(ctx, val)
			prom.Add(ctx, val)
			lg.LogAttrs(ctx, slog.LevelInfo, "canary message",
				slog.Bool("is_log_canary", true),
			)
		}
	}
}

func initOTLPExporter(ctx context.Context) (ctr, pctr instrument.Int64Counter, err error) {
	serviceConfig := `{"loadBalancingConfig":[{"round_robin":{}}]}`
	metricExporter, err := otlpmetricgrpc.New( // export config from env
		ctx,
		otlpmetricgrpc.WithServiceConfig(serviceConfig),
		otlpmetricgrpc.WithTemporalitySelector(func(ik metric.InstrumentKind) metricdata.Temporality {
			return metricdata.DeltaTemporality // delta for Datadog
		}),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create otlp exporter: %w", err)
	}

	meterProvider := metric.NewMeterProvider(
		metric.WithReader(
			metric.NewPeriodicReader(
				metricExporter,
				metric.WithInterval(10*time.Second),
			),
		),
		otelHTTPView(),
	)

	meter := meterProvider.Meter(Name)
	ctr, _ = meter.Int64Counter("polaris.o11y.canary.otlp")
	pctr, _ = meter.Int64Counter("polaris.o11y.canary.promhandler")
	return ctr, pctr, nil
}

func initPromExporter(ctx context.Context) (ctr, pctr instrument.Int64Counter, h http.Handler, err error) {
	promReg := prometheus.NewRegistry()
	metricExporter, err := otelprometheus.New(
		otelprometheus.WithoutScopeInfo(),
		otelprometheus.WithoutTargetInfo(),
		otelprometheus.WithoutUnits(),
		otelprometheus.WithRegisterer(promReg),
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create prom exporter: %w", err)
	}

	meterProvider := metric.NewMeterProvider(
		metric.WithReader(metricExporter),
		otelHTTPView(),
	)

	meter := meterProvider.Meter(Name)
	ctr, _ = meter.Int64Counter("polaris.o11y.canary.prom")
	pctr, _ = meter.Int64Counter("polaris.o11y.canary.promhandler")
	return ctr, pctr, promhttp.HandlerFor(promReg, promhttp.HandlerOpts{}), nil
}

// https://github.com/open-telemetry/opentelemetry-go-contrib/issues/3071
func otelHTTPView() metric.Option {
	return metric.WithView(
		metric.NewView(
			metric.Instrument{
				Scope: instrumentation.Scope{
					Name: "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp",
				},
			}, metric.Stream{
				AttributeFilter: attribute.Filter(func(kv attribute.KeyValue) bool {
					switch kv.Key {
					case "net.sock.peer.addr", "net.sock.peer.port":
						return false
					default:
						return true
					}
				}),
			},
		),
	)
}
