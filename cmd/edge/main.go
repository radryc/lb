package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
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
	"github.com/rydzu/ainfra/lb/pkg/proxy"
	"github.com/rydzu/ainfra/lb/pkg/registry"
	"github.com/rydzu/ainfra/lb/pkg/telemetry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/reflection"
)

func main() {
	var (
		bindAddress   = flag.String("bind-address", envOrDefault("LB_BIND_ADDRESS", "0.0.0.0"), "listener bind address")
		serviceBase   = flag.String("service-base", envOrDefault("LB_SERVICE_BASE", "edge"), "service name prefix for bootstrapped listeners")
		account       = flag.String("account", envOrDefault("LB_ACCOUNT", "default"), "account/tenant for bootstrapped services")
		bootstrap     = flag.String("bootstrap", envOrDefault("LB_BOOTSTRAP", ""), "bootstrap mapping: <external_port>=host:port[,host:port][;<external_port>=...]")
		metricsAddr   = flag.String("metrics-addr", envOrDefault("LB_METRICS_ADDR", ":19090"), "Prometheus metrics listen address")
		registryHTTP  = flag.String("registry-http-addr", envOrDefault("LB_REGISTRY_HTTP_ADDR", "127.0.0.1:18081"), "local registry HTTP listen address")
		registryGRPC  = flag.String("registry-grpc-addr", envOrDefault("LB_REGISTRY_GRPC_ADDR", "0.0.0.0:15051"), "local registry gRPC listen address")
		syncEvery     = flag.Duration("sync-every", durationFromEnv("LB_SYNC_EVERY", 2*time.Second), "state sync interval")
		dialTimeout   = flag.Duration("dial-timeout", durationFromEnv("LB_DIAL_TIMEOUT", 3*time.Second), "backend dial timeout")
		minPort       = flag.Int("min-external-port", 1, "minimum dynamic external port")
		maxPort       = flag.Int("max-external-port", 65535, "maximum dynamic external port")
		proxyProtocol = flag.Bool("proxy-protocol", boolFromEnv("LB_PROXY_PROTOCOL", false), "send PROXY protocol v1 header to backends")
		authToken     = flag.String("auth-token", envOrDefault("LB_REGISTRY_TOKEN", ""), "optional bearer token required for registry mutations and HTTP endpoints")
		tlsCertFile   = flag.String("tls-cert-file", envOrDefault("LB_TLS_CERT_FILE", ""), "optional TLS certificate file for registry gRPC and HTTP")
		tlsKeyFile    = flag.String("tls-key-file", envOrDefault("LB_TLS_KEY_FILE", ""), "optional TLS private key file for registry gRPC and HTTP")
		registryCA    = flag.String("registry-ca-file", envOrDefault("LB_REGISTRY_CA_FILE", ""), "optional CA bundle used to verify the registry TLS certificate (defaults to system roots)")
		skipVerify    = flag.Bool("insecure-skip-verify", boolFromEnv("LB_INSECURE_SKIP_VERIFY", false), "disable registry TLS certificate verification (development only)")
	)
	flag.Parse()

	component := "lb-edge"
	otelEndpoint := envFirst("LB_OTEL_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT")
	telemetryHandle, err := lbtelemetry.Setup(context.Background(), lbtelemetry.Config{
		Endpoint:       otelEndpoint,
		ServiceName:    lbtelemetry.ServiceName("lb.invalid.unconfigured", "OTEL_SERVICE_NAME", "LB_OTEL_SERVICE_NAME"),
		Component:      component,
		InstanceID:     envOrDefault("HOSTNAME", component),
		Insecure:       true,
		MetricInterval: durationFromEnv("LB_OTEL_METRIC_INTERVAL", 15*time.Second),
	})
	if err != nil {
		log.Fatalf("setup telemetry: %v", err)
	}
	if telemetryHandle.Enabled() {
		log.SetOutput(io.MultiWriter(os.Stderr, lbtelemetry.NewStdLogWriter("lb/edge")))
		lbtelemetry.EmitInfo(context.Background(), "lb/edge", "lb-edge telemetry enabled")
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := telemetryHandle.Shutdown(shutdownCtx); err != nil {
				log.Printf("shutdown telemetry: %v", err)
			}
		}()
	}
	log.Printf("starting lb-edge version=%s commit=%s", buildinfo.Version, buildinfo.Commit)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	state := registry.NewState(int32(*minPort), int32(*maxPort))
	if err := seedBootstrap(state, *account, *serviceBase, *bootstrap); err != nil {
		log.Fatalf("bootstrap invalid: %v", err)
	}

	certFile := strings.TrimSpace(*tlsCertFile)
	keyFile := strings.TrimSpace(*tlsKeyFile)
	if (certFile == "") != (keyFile == "") {
		log.Fatalf("both --tls-cert-file and --tls-key-file must be set together")
	}

	grpcOpts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(registry.BearerAuthUnaryInterceptor(*authToken)),
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
	grpcLis, err := net.Listen("tcp", *registryGRPC)
	if err != nil {
		log.Fatalf("listen registry grpc %s: %v", *registryGRPC, err)
	}
	go func() {
		if err := grpcServer.Serve(grpcLis); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("registry grpc exited: %v", err)
		}
	}()

	httpSrv := &http.Server{
		Addr:              *registryHTTP,
		Handler:           registry.HTTPServicesHandlerWithAuth(state, *authToken),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		var serveErr error
		if certFile != "" {
			serveErr = httpSrv.ListenAndServeTLS(certFile, keyFile)
		} else {
			serveErr = httpSrv.ListenAndServe()
		}
		if serveErr != nil && serveErr != http.ErrServerClosed {
			log.Printf("registry http exited: %v", serveErr)
		}
	}()

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

	registryScheme := "http://"
	if certFile != "" {
		registryScheme = "https://"
	}
	engine := proxy.NewEngine(*bindAddress, registryScheme+*registryHTTP, *syncEvery, *dialTimeout, metrics, *proxyProtocol)
	if strings.TrimSpace(*authToken) != "" {
		engine.SetRegistryToken(*authToken)
	}
	if certFile != "" {
		registryTLS, err := registryTLSConfig(*registryCA, *skipVerify)
		if err != nil {
			log.Fatalf("configure registry TLS client: %v", err)
		}
		if *skipVerify {
			log.Printf("WARNING: registry TLS certificate verification is disabled (LB_INSECURE_SKIP_VERIFY=true); do not use in production")
		}
		engine.SetHTTPClient(&http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: registryTLS,
			},
		})
	}
	go func() {
		if err := engine.Run(ctx); err != nil {
			log.Printf("proxy engine exited: %v", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	grpcServer.GracefulStop()
	_ = httpSrv.Shutdown(shutdownCtx)
}

func seedBootstrap(state *registry.State, account, serviceBase, bootstrap string) error {
	bootstrap = strings.TrimSpace(bootstrap)
	if bootstrap == "" {
		return nil
	}
	listenerSpecs := strings.Split(bootstrap, ";")
	for _, rawListener := range listenerSpecs {
		rawListener = strings.TrimSpace(rawListener)
		if rawListener == "" {
			continue
		}
		parts := strings.SplitN(rawListener, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid listener mapping %q", rawListener)
		}
		lhs := strings.TrimSpace(parts[0])
		rawBackendList := parts[1]

		// Parse lhs: [name[@protocol][description]:]port
		// Formats supported:
		//   9090                             → name=edge-9090, no protocol
		//   grpc@grpc:9090                  → name=grpc, protocol=grpc
		//   monofs-http@http[MonoFS UI]:8080 → name, protocol, description
		var (
			serviceName string
			protocol    string
			description string
			portStr     string
		)

		// Split on last ':' to get port
		if colonIdx := strings.LastIndex(lhs, ":"); colonIdx >= 0 {
			portStr = lhs[colonIdx+1:]
			nameProto := lhs[:colonIdx]

			// Extract optional [description]
			if lb := strings.Index(nameProto, "["); lb >= 0 {
				rb := strings.LastIndex(nameProto, "]")
				if rb > lb {
					description = nameProto[lb+1 : rb]
					nameProto = nameProto[:lb] + nameProto[rb+1:]
				}
			}

			// Split name@protocol
			if atIdx := strings.Index(nameProto, "@"); atIdx >= 0 {
				serviceName = strings.TrimSpace(nameProto[:atIdx])
				protocol = strings.TrimSpace(nameProto[atIdx+1:])
			} else {
				serviceName = strings.TrimSpace(nameProto)
			}
		} else {
			portStr = lhs
		}

		externalPort, err := strconv.Atoi(strings.TrimSpace(portStr))
		if err != nil || externalPort <= 0 {
			return fmt.Errorf("invalid external port in mapping %q", rawListener)
		}
		if serviceName == "" {
			serviceName = fmt.Sprintf("%s-%d", serviceBase, externalPort)
		}

		rawBackends := strings.Split(rawBackendList, ",")
		for _, rawBackend := range rawBackends {
			rawBackend = strings.TrimSpace(rawBackend)
			if rawBackend == "" {
				continue
			}
			host, port, err := splitHostPort(rawBackend)
			if err != nil {
				return fmt.Errorf("invalid backend %q for external port %d: %w", rawBackend, externalPort, err)
			}
			if err := state.Seed(account, serviceName, protocol, description, int32(externalPort), host, int32(port)); err != nil {
				return fmt.Errorf("seed bootstrap backend %s for %d failed: %w", rawBackend, externalPort, err)
			}
		}
	}
	return nil
}

func splitHostPort(raw string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(raw)
	if err != nil {
		if strings.Count(raw, ":") == 1 && !strings.HasPrefix(raw, "[") {
			idx := strings.LastIndex(raw, ":")
			host = raw[:idx]
			portStr = raw[idx+1:]
		} else {
			return "", 0, err
		}
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, err
	}
	if port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("port %d out of range", port)
	}
	return host, port, nil
}

// registryTLSConfig builds the TLS client configuration used to poll the
// registry HTTP API. Certificates are verified against the system trust store
// by default; caFile optionally replaces it with a custom CA bundle.
// Verification is only skipped when the caller explicitly opts in.
func registryTLSConfig(caFile string, insecureSkipVerify bool) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if insecureSkipVerify {
		cfg.InsecureSkipVerify = true
		return cfg, nil
	}
	caFile = strings.TrimSpace(caFile)
	if caFile == "" {
		// nil RootCAs means "use the system trust store".
		return cfg, nil
	}
	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA file %s: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("CA file %s contains no usable certificates", caFile)
	}
	cfg.RootCAs = pool
	return cfg, nil
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

func durationFromEnv(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return d
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
