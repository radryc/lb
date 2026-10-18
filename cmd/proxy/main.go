package main

import (
	"context"
	"flag"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rydzu/ainfra/lb/internal/buildinfo"
	lbtelemetry "github.com/rydzu/ainfra/lb/internal/telemetry"
	"github.com/rydzu/ainfra/lb/pkg/proxy"
	"github.com/rydzu/ainfra/lb/pkg/telemetry"
)

func main() {
	var (
		bindAddress   = flag.String("bind-address", "0.0.0.0", "listener bind address")
		registryURL   = flag.String("registry-url", "http://127.0.0.1:8081", "registry HTTP base URL")
		syncEvery     = flag.Duration("sync-every", 2*time.Second, "state sync interval")
		dialTimeout   = flag.Duration("dial-timeout", 3*time.Second, "backend dial timeout")
		metricsAddr   = flag.String("metrics-addr", ":9090", "Prometheus metrics listen address")
		proxyProtocol = flag.Bool("proxy-protocol", false, "send PROXY protocol v1 header to backends")
	)
	flag.Parse()

	component := "lb-proxy"
	otelEndpoint := envFirst("LB_OTEL_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT")
	telemetryHandle, err := lbtelemetry.Setup(context.Background(), lbtelemetry.Config{
		Endpoint:       otelEndpoint,
		ServiceName:    lbtelemetry.ServiceName("lb", "OTEL_SERVICE_NAME", "LB_OTEL_SERVICE_NAME"),
		Component:      component,
		InstanceID:     envOrDefault("HOSTNAME", component),
		Insecure:       true,
		MetricInterval: 15 * time.Second,
	})
	if err != nil {
		log.Fatalf("setup telemetry: %v", err)
	}
	if telemetryHandle.Enabled() {
		log.SetOutput(io.MultiWriter(os.Stderr, lbtelemetry.NewStdLogWriter("lb/proxy")))
		lbtelemetry.EmitInfo(context.Background(), "lb/proxy", "lb-proxy telemetry enabled")
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := telemetryHandle.Shutdown(shutdownCtx); err != nil {
				log.Printf("shutdown telemetry: %v", err)
			}
		}()
	}
	log.Printf("starting lb-proxy version=%s commit=%s", buildinfo.Version, buildinfo.Commit)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	tel, err := telemetry.InitMetrics(ctx, *metricsAddr)
	if err != nil {
		log.Fatalf("init telemetry: %v", err)
	}
	defer func() {
		_ = tel.Shutdown(context.Background())
	}()

	metrics, err := proxy.NewProxyMetrics(tel.Meter)
	if err != nil {
		log.Fatalf("create proxy metrics instruments: %v", err)
	}

	engine := proxy.NewEngine(*bindAddress, *registryURL, *syncEvery, *dialTimeout, metrics, *proxyProtocol)
	log.Printf("proxy syncing from %s and exposing metrics at %s", *registryURL, *metricsAddr)
	if err := engine.Run(ctx); err != nil {
		log.Fatalf("proxy exited: %v", err)
	}
}

func envFirst(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
