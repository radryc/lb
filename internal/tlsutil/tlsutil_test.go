package tlsutil

import (
	"testing"
	"time"

	"github.com/radryc/tlsx/pki"
)

func generateBundle(t *testing.T) *pki.Bundle {
	t.Helper()
	bundle, err := pki.Generate(pki.Options{
		Dir:         t.TempDir(),
		TrustDomain: "strata.test",
		Validity:    time.Hour,
		Services: []pki.Service{
			{Name: "svc", Namespace: "lb", IPs: []string{"127.0.0.1"}},
		},
	})
	if err != nil {
		t.Fatalf("pki.Generate: %v", err)
	}
	return bundle
}

func clearStrataEnv(t *testing.T) {
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

func TestResolvePlaintextDefault(t *testing.T) {
	clearStrataEnv(t)

	settings, err := Resolve(Legacy{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if settings.ServerEnabled() || settings.ClientEnabled() {
		t.Fatal("no configuration must leave TLS disabled")
	}
}

func TestResolveLegacyCertificate(t *testing.T) {
	clearStrataEnv(t)
	bundle := generateBundle(t)
	identity := bundle.Services["svc"]

	settings, err := Resolve(Legacy{CertFile: identity.CertFile, KeyFile: identity.KeyFile})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !settings.ServerEnabled() || !settings.ClientEnabled() {
		t.Fatal("legacy certificate must enable TLS")
	}
	if settings.Mode != "off" {
		t.Fatalf("mode = %q, want off for the legacy path", settings.Mode)
	}
}

func TestResolvePrefersStrataEnvOverLegacy(t *testing.T) {
	bundle := generateBundle(t)
	identity := bundle.Services["svc"]
	t.Setenv("STRATA_TLS_MODE", "required")
	t.Setenv("STRATA_TLS_CERT_FILE", identity.CertFile)
	t.Setenv("STRATA_TLS_KEY_FILE", identity.KeyFile)
	t.Setenv("STRATA_TLS_CA_FILE", bundle.CAFile)
	t.Setenv("STRATA_TLS_SERVER_NAME", "localhost")
	t.Setenv("STRATA_TLS_REQUIRE_CLIENT_CERT", "false")

	settings, err := Resolve(Legacy{
		CertFile:           "/does/not/exist.crt",
		KeyFile:            "/does/not/exist.key",
		InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if settings.Source != "STRATA_TLS_*" {
		t.Fatalf("source = %q, want STRATA_TLS_*", settings.Source)
	}
	if settings.Client == nil || settings.Client.InsecureSkipVerify {
		t.Fatal("STRATA_TLS_* must win and must never disable verification")
	}
	if len(settings.Warnings) == 0 {
		t.Fatal("expected warnings about ignored legacy flags")
	}
}

func TestResolveRejectsInvalidMode(t *testing.T) {
	clearStrataEnv(t)
	t.Setenv("STRATA_TLS_MODE", "sometimes")

	if _, err := Resolve(Legacy{}); err == nil {
		t.Fatal("expected an error for an invalid STRATA_TLS_MODE")
	}
}
