package main

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/radryc/tlsx"
	"github.com/radryc/tlsx/pki"
	"github.com/rydzu/ainfra/lb/internal/tlsutil"
	"github.com/rydzu/ainfra/lb/pkg/pb"
	"github.com/rydzu/ainfra/lb/pkg/registry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

const testAuthToken = "registry-secret"

// generateRegistryPKI issues a CA plus the registry and client identities used
// by the TLS tests.
func generateRegistryPKI(t *testing.T) *pki.Bundle {
	t.Helper()
	bundle, err := pki.Generate(pki.Options{
		Dir:         t.TempDir(),
		TrustDomain: "strata.test",
		Validity:    time.Hour,
		Services: []pki.Service{
			{Name: "registry", Namespace: "lb", IPs: []string{"127.0.0.1"}},
			{Name: "client", Namespace: "lb"},
		},
	})
	if err != nil {
		t.Fatalf("pki.Generate: %v", err)
	}
	return bundle
}

// configureStrataTLS points the STRATA_TLS_* contract at the generated PKI.
func configureStrataTLS(t *testing.T, bundle *pki.Bundle, service string, requireClientCert bool) {
	t.Helper()
	identity := bundle.Services[service]
	t.Setenv("STRATA_TLS_MODE", "required")
	t.Setenv("STRATA_TLS_CERT_FILE", identity.CertFile)
	t.Setenv("STRATA_TLS_KEY_FILE", identity.KeyFile)
	t.Setenv("STRATA_TLS_CA_FILE", bundle.CAFile)
	t.Setenv("STRATA_TLS_SERVER_NAME", "localhost")
	t.Setenv("STRATA_TLS_REQUIRE_CLIENT_CERT", strconv.FormatBool(requireClientCert))
	t.Setenv("STRATA_TLS_ALLOWED_SPIFFE_IDS", "")
	t.Setenv("STRATA_TLS_EXPECTED_SPIFFE_IDS", "")
}

func clearStrataTLS(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"STRATA_TLS_MODE",
		"STRATA_TLS_CERT_FILE",
		"STRATA_TLS_KEY_FILE",
		"STRATA_TLS_CA_FILE",
		"STRATA_TLS_SERVER_NAME",
		"STRATA_TLS_REQUIRE_CLIENT_CERT",
		"STRATA_TLS_ALLOWED_SPIFFE_IDS",
		"STRATA_TLS_EXPECTED_SPIFFE_IDS",
	} {
		t.Setenv(key, "")
	}
}

func clientCredentials(t *testing.T, bundle *pki.Bundle, service string) credentials.TransportCredentials {
	t.Helper()
	identity := bundle.Services[service]
	cfg, err := tlsx.NewClientTLSConfig(tlsx.ClientConfig{
		CertFile:   identity.CertFile,
		KeyFile:    identity.KeyFile,
		CAFile:     bundle.CAFile,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatalf("client TLS config: %v", err)
	}
	return credentials.NewTLS(cfg)
}

func serveRegistryGRPC(t *testing.T, tlsCfg *tls.Config) (string, *grpc.Server) {
	t.Helper()
	state := registry.NewState(10000, 20000)
	srv := newRegistryGRPCServer(state, testAuthToken, tlsCfg)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
	})
	return lis.Addr().String(), srv
}

func TestRegistryGRPCTLSRoundTrip(t *testing.T) {
	bundle := generateRegistryPKI(t)
	configureStrataTLS(t, bundle, "registry", true)

	settings, err := tlsutil.Resolve(tlsutil.Legacy{})
	if err != nil {
		t.Fatalf("tlsutil.Resolve: %v", err)
	}
	if !settings.ServerEnabled() || !settings.ClientEnabled() {
		t.Fatal("required mode must configure both server and client TLS")
	}

	addr, _ := serveRegistryGRPC(t, settings.Server)
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(clientCredentials(t, bundle, "client")))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+testAuthToken)
	if _, err := pb.NewDiscoveryRegistryClient(conn).ListServices(ctx, &pb.ListServicesRequest{}); err != nil {
		t.Fatalf("ListServices over mTLS: %v", err)
	}
}

func TestRegistryGRPCRejectsPlaintextInRequiredMode(t *testing.T) {
	bundle := generateRegistryPKI(t)
	configureStrataTLS(t, bundle, "registry", true)

	settings, err := tlsutil.Resolve(tlsutil.Legacy{})
	if err != nil {
		t.Fatalf("tlsutil.Resolve: %v", err)
	}
	addr, _ := serveRegistryGRPC(t, settings.Server)

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+testAuthToken)
	if _, err := pb.NewDiscoveryRegistryClient(conn).ListServices(ctx, &pb.ListServicesRequest{}); err == nil {
		t.Fatal("plaintext RPC must fail against a TLS registry")
	}
}

func TestRegistryGRPCRejectsClientWithoutCertificate(t *testing.T) {
	bundle := generateRegistryPKI(t)
	configureStrataTLS(t, bundle, "registry", true)

	settings, err := tlsutil.Resolve(tlsutil.Legacy{})
	if err != nil {
		t.Fatalf("tlsutil.Resolve: %v", err)
	}
	addr, _ := serveRegistryGRPC(t, settings.Server)

	anonymous, err := tlsx.NewClientTLSConfig(tlsx.ClientConfig{
		CAFile:     bundle.CAFile,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatalf("anonymous client TLS config: %v", err)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(anonymous)))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+testAuthToken)
	if _, err := pb.NewDiscoveryRegistryClient(conn).ListServices(ctx, &pb.ListServicesRequest{}); err == nil {
		t.Fatal("mTLS registry must reject clients without a certificate")
	}
}

func TestRegistryHTTPTLSRoundTrip(t *testing.T) {
	bundle := generateRegistryPKI(t)
	configureStrataTLS(t, bundle, "registry", true)

	settings, err := tlsutil.Resolve(tlsutil.Legacy{})
	if err != nil {
		t.Fatalf("tlsutil.Resolve: %v", err)
	}

	state := registry.NewState(10000, 20000)
	httpSrv := newRegistryHTTPServer("127.0.0.1:0", state, testAuthToken)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = httpSrv.Serve(tls.NewListener(lis, settings.Server)) }()
	t.Cleanup(func() { _ = httpSrv.Close() })

	clientTLS, err := tlsx.NewClientTLSConfig(tlsx.ClientConfig{
		CertFile:   bundle.Services["client"].CertFile,
		KeyFile:    bundle.Services["client"].KeyFile,
		CAFile:     bundle.CAFile,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatalf("client TLS config: %v", err)
	}
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: clientTLS},
	}
	resp, err := client.Get("https://" + lis.Addr().String() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz over TLS: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("healthz = %d %q, want 200 ok", resp.StatusCode, body)
	}
}

func TestRegistryHTTPRejectsPlaintextInRequiredMode(t *testing.T) {
	bundle := generateRegistryPKI(t)
	configureStrataTLS(t, bundle, "registry", true)

	settings, err := tlsutil.Resolve(tlsutil.Legacy{})
	if err != nil {
		t.Fatalf("tlsutil.Resolve: %v", err)
	}

	state := registry.NewState(10000, 20000)
	httpSrv := newRegistryHTTPServer("127.0.0.1:0", state, testAuthToken)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = httpSrv.Serve(tls.NewListener(lis, settings.Server)) }()
	t.Cleanup(func() { _ = httpSrv.Close() })

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + lis.Addr().String() + "/healthz")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// crypto/tls answers a plaintext HTTP request with a 400 before the
	// handshake completes, so a non-200 response also proves the endpoint is
	// not serving plaintext.
	if resp.StatusCode == http.StatusOK && string(body) == "ok" {
		t.Fatal("plaintext HTTP request must not reach the TLS registry")
	}
}

func TestResolveDefaultsToPlaintextWhenUnconfigured(t *testing.T) {
	clearStrataTLS(t)

	settings, err := tlsutil.Resolve(tlsutil.Legacy{})
	if err != nil {
		t.Fatalf("tlsutil.Resolve: %v", err)
	}
	if settings.ServerEnabled() || settings.ClientEnabled() {
		t.Fatal("unconfigured STRATA_TLS_* must keep the plaintext default")
	}
	if settings.Mode != tlsx.ModeOff {
		t.Fatalf("mode = %q, want off", settings.Mode)
	}
}
