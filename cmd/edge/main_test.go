package main

import (
	"crypto/tls"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func newTLSRegistryServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func tlsClient(cfg *tls.Config) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
}

func TestRegistryTLSConfigVerifiesByDefault(t *testing.T) {
	srv := newTLSRegistryServer(t)

	cfg, err := registryTLSConfig("", false)
	if err != nil {
		t.Fatalf("registryTLSConfig: %v", err)
	}
	if cfg.InsecureSkipVerify {
		t.Fatal("default config must not skip verification")
	}
	if cfg.RootCAs != nil {
		t.Fatal("default config must use the system trust store")
	}

	// The httptest certificate is self-signed, so verification must fail.
	if resp, err := tlsClient(cfg).Get(srv.URL); err == nil {
		resp.Body.Close()
		t.Fatal("expected certificate verification failure against a self-signed server")
	}
}

func TestRegistryTLSConfigCustomCA(t *testing.T) {
	srv := newTLSRegistryServer(t)

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatalf("write CA file: %v", err)
	}

	cfg, err := registryTLSConfig(caFile, false)
	if err != nil {
		t.Fatalf("registryTLSConfig: %v", err)
	}
	if cfg.InsecureSkipVerify {
		t.Fatal("custom CA must still verify the server certificate")
	}
	if cfg.RootCAs == nil {
		t.Fatal("custom CA should populate RootCAs")
	}

	resp, err := tlsClient(cfg).Get(srv.URL)
	if err != nil {
		t.Fatalf("GET with custom CA: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestRegistryTLSConfigInsecureSkipVerifyOptIn(t *testing.T) {
	srv := newTLSRegistryServer(t)

	cfg, err := registryTLSConfig("", true)
	if err != nil {
		t.Fatalf("registryTLSConfig: %v", err)
	}
	if !cfg.InsecureSkipVerify {
		t.Fatal("explicit opt-in should disable verification")
	}

	resp, err := tlsClient(cfg).Get(srv.URL)
	if err != nil {
		t.Fatalf("GET with skip-verify: %v", err)
	}
	resp.Body.Close()
}

func TestRegistryTLSConfigRejectsBadCA(t *testing.T) {
	if _, err := registryTLSConfig(filepath.Join(t.TempDir(), "missing.pem"), false); err == nil {
		t.Fatal("expected error for missing CA file")
	}

	badFile := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(badFile, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write bad CA file: %v", err)
	}
	if _, err := registryTLSConfig(badFile, false); err == nil {
		t.Fatal("expected error for CA file without certificates")
	}
}
