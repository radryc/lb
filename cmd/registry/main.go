package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rydzu/ainfra/lb/internal/buildinfo"
	lbtelemetry "github.com/rydzu/ainfra/lb/internal/telemetry"
	"github.com/rydzu/ainfra/lb/pkg/pb"
	"github.com/rydzu/ainfra/lb/pkg/registry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

func main() {
	var (
		grpcAddr = flag.String("grpc-addr", ":50051", "gRPC listen address")
		httpAddr = flag.String("http-addr", ":8081", "HTTP listen address")
		minPort  = flag.Int("min-external-port", 10000, "minimum dynamic external port")
		maxPort  = flag.Int("max-external-port", 60000, "maximum dynamic external port")
	)
	flag.Parse()

	component := "lb-registry"
	otelEndpoint := envFirst("LB_OTEL_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT")
	telemetryHandle, err := lbtelemetry.Setup(context.Background(), lbtelemetry.Config{
		Endpoint:       otelEndpoint,
		ServiceName:    envOrDefault("LB_OTEL_SERVICE_NAME", "lb"),
		Component:      component,
		InstanceID:     envOrDefault("HOSTNAME", component),
		Insecure:       true,
		MetricInterval: 15 * time.Second,
	})
	if err != nil {
		log.Fatalf("setup telemetry: %v", err)
	}
	if telemetryHandle.Enabled() {
		log.SetOutput(io.MultiWriter(os.Stderr, lbtelemetry.NewStdLogWriter("lb/registry")))
		lbtelemetry.EmitInfo(context.Background(), "lb/registry", "lb-registry telemetry enabled")
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := telemetryHandle.Shutdown(shutdownCtx); err != nil {
				log.Printf("shutdown telemetry: %v", err)
			}
		}()
	}
	log.Printf("starting lb-registry version=%s commit=%s", buildinfo.Version, buildinfo.Commit)

	state := registry.NewState(int32(*minPort), int32(*maxPort))
	grpcServer := grpc.NewServer()
	pb.RegisterDiscoveryRegistryServer(grpcServer, registry.NewGRPCServer(state))
	reflection.Register(grpcServer)

	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Fatalf("listen grpc %s: %v", *grpcAddr, err)
	}

	httpSrv := &http.Server{
		Addr:    *httpAddr,
		Handler: registry.HTTPServicesHandler(state),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("registry gRPC listening on %s", *grpcAddr)
		if err := grpcServer.Serve(lis); err != nil {
			log.Printf("grpc serve stopped: %v", err)
		}
	}()

	go func() {
		log.Printf("registry HTTP listening on %s", *httpAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("http serve stopped: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down registry")
	grpcServer.GracefulStop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
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
