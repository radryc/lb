package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rydzu/ainfra/lb/internal/buildinfo"
	lbtelemetry "github.com/rydzu/ainfra/lb/internal/telemetry"
	"github.com/rydzu/ainfra/lb/pkg/pb"
	"github.com/rydzu/ainfra/lb/pkg/registry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/reflection"
)

func main() {
	var (
		grpcAddr    = flag.String("grpc-addr", ":50051", "gRPC listen address")
		httpAddr    = flag.String("http-addr", ":8081", "HTTP listen address")
		minPort     = flag.Int("min-external-port", 10000, "minimum dynamic external port")
		maxPort     = flag.Int("max-external-port", 60000, "maximum dynamic external port")
		authToken   = flag.String("auth-token", envOrDefault("LB_REGISTRY_TOKEN", ""), "bearer token required for registry RPCs and HTTP endpoints (required unless --allow-anonymous)")
		allowAnon   = flag.Bool("allow-anonymous", boolFromEnv("LB_ALLOW_ANONYMOUS", false), "allow unauthenticated registry access (development only)")
		tlsCertFile = flag.String("tls-cert-file", envOrDefault("LB_TLS_CERT_FILE", ""), "optional TLS certificate file for gRPC and HTTP")
		tlsKeyFile  = flag.String("tls-key-file", envOrDefault("LB_TLS_KEY_FILE", ""), "optional TLS private key file for gRPC and HTTP")
	)
	flag.Parse()

	authTokenValue := strings.TrimSpace(*authToken)
	if err := validateAuthConfig(authTokenValue, *allowAnon); err != nil {
		log.Fatalf("%v", err)
	}
	if authTokenValue == "" {
		log.Printf("WARNING: registry authentication is disabled (--allow-anonymous / LB_ALLOW_ANONYMOUS=true); do not expose this registry")
	}

	component := "lb-registry"
	otelEndpoint := envFirst("LB_OTEL_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT")
	telemetryHandle, err := lbtelemetry.Setup(context.Background(), lbtelemetry.Config{
		Endpoint:       otelEndpoint,
		ServiceName:    lbtelemetry.ServiceName("lb.invalid.unconfigured", "OTEL_SERVICE_NAME", "LB_OTEL_SERVICE_NAME"),
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

	certFile := strings.TrimSpace(*tlsCertFile)
	keyFile := strings.TrimSpace(*tlsKeyFile)
	if (certFile == "") != (keyFile == "") {
		log.Fatalf("both --tls-cert-file and --tls-key-file must be set together")
	}

	state := registry.NewState(int32(*minPort), int32(*maxPort))
	grpcOpts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(registry.BearerAuthUnaryInterceptor(authTokenValue)),
	}
	if certFile != "" {
		creds, err := credentials.NewServerTLSFromFile(certFile, keyFile)
		if err != nil {
			log.Fatalf("load TLS credentials: %v", err)
		}
		grpcOpts = append(grpcOpts, grpc.Creds(creds))
	}
	grpcServer := grpc.NewServer(grpcOpts...)
	pb.RegisterDiscoveryRegistryServer(grpcServer, registry.NewGRPCServer(state))
	reflection.Register(grpcServer)

	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Fatalf("listen grpc %s: %v", *grpcAddr, err)
	}

	httpSrv := &http.Server{
		Addr:              *httpAddr,
		Handler:           registry.HTTPServicesHandlerWithAuth(state, authTokenValue),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("registry gRPC listening on %s (tls=%v, auth=%v)", *grpcAddr, certFile != "", authTokenValue != "")
		if err := grpcServer.Serve(lis); err != nil {
			log.Printf("grpc serve stopped: %v", err)
		}
	}()

	go func() {
		log.Printf("registry HTTP listening on %s (tls=%v, auth=%v)", *httpAddr, certFile != "", authTokenValue != "")
		var serveErr error
		if certFile != "" {
			serveErr = httpSrv.ListenAndServeTLS(certFile, keyFile)
		} else {
			serveErr = httpSrv.ListenAndServe()
		}
		if serveErr != nil && serveErr != http.ErrServerClosed {
			log.Printf("http serve stopped: %v", serveErr)
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

// validateAuthConfig rejects configurations that would silently serve the
// registry without authentication. Anonymous access must be opted into
// explicitly, which keeps the default deployment fail-closed.
func validateAuthConfig(authToken string, allowAnonymous bool) error {
	if strings.TrimSpace(authToken) != "" || allowAnonymous {
		return nil
	}
	return fmt.Errorf("registry bearer token is required: set --auth-token or LB_REGISTRY_TOKEN (use --allow-anonymous or LB_ALLOW_ANONYMOUS=true for unauthenticated development only)")
}

func boolFromEnv(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return b
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
