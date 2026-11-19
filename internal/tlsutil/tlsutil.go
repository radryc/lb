// Package tlsutil resolves the TLS/mTLS configuration for the lb binaries.
//
// The canonical contract is the shared STRATA_TLS_* environment (see
// github.com/radryc/tlsx). The legacy LB_TLS_CERT_FILE / LB_TLS_KEY_FILE and
// LB_REGISTRY_CA_FILE flags keep working when STRATA_TLS_MODE is unset, so
// existing deployments stay on their current behaviour.
package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"

	"github.com/radryc/tlsx"
)

// Legacy describes the pre-STRATA_TLS_* TLS configuration of an lb binary.
type Legacy struct {
	// CertFile and KeyFile enable one-way TLS on the service listeners.
	CertFile string
	KeyFile  string
	// ClientCAFile optionally replaces the system trust store when dialing
	// outbound TLS endpoints (LB_REGISTRY_CA_FILE).
	ClientCAFile string
	// InsecureSkipVerify disables outbound certificate verification. It is a
	// development-only escape hatch and is ignored whenever STRATA_TLS_MODE is
	// set, because the shared contract never disables verification.
	InsecureSkipVerify bool
}

// Settings is the resolved TLS configuration of a process.
type Settings struct {
	// Mode is the effective STRATA_TLS_MODE. It stays "off" when the legacy
	// flags provided TLS, which keeps the legacy path distinguishable.
	Mode tlsx.Mode
	// Server is non-nil when the process must serve TLS.
	Server *tls.Config
	// Client is non-nil when outbound connections must use TLS.
	Client *tls.Config
	// Source describes where the configuration came from, for startup logs.
	Source string
	// Warnings holds non-fatal configuration notes for startup logs.
	Warnings []string
}

// ServerEnabled reports whether the process must serve TLS.
func (s Settings) ServerEnabled() bool { return s.Server != nil }

// ClientEnabled reports whether outbound connections must use TLS.
func (s Settings) ClientEnabled() bool { return s.Client != nil }

// Resolve builds the TLS configuration for a binary from STRATA_TLS_* with a
// fallback to the legacy flags. With no configuration at all it returns a
// plaintext Settings, preserving the historic default.
func Resolve(legacy Legacy) (Settings, error) {
	cfg, err := tlsx.LoadEnv()
	if err != nil {
		return Settings{}, err
	}
	if cfg.Mode != tlsx.ModeOff {
		settings := Settings{Mode: cfg.Mode, Source: "STRATA_TLS_*"}
		settings.Server, err = tlsx.NewServerTLSConfig(cfg.ServerConfig())
		if err != nil {
			return Settings{}, fmt.Errorf("strata tls server: %w", err)
		}
		settings.Client, err = tlsx.NewClientTLSConfig(cfg.ClientConfig())
		if err != nil {
			return Settings{}, fmt.Errorf("strata tls client: %w", err)
		}
		if legacy.InsecureSkipVerify {
			settings.Warnings = append(settings.Warnings,
				"LB_INSECURE_SKIP_VERIFY is ignored while STRATA_TLS_MODE is set; peer certificates are always verified")
		}
		if strings.TrimSpace(legacy.CertFile) != "" {
			settings.Warnings = append(settings.Warnings,
				"STRATA_TLS_* overrides --tls-cert-file/--tls-key-file and LB_REGISTRY_CA_FILE")
		}
		return settings, nil
	}

	certFile := strings.TrimSpace(legacy.CertFile)
	if certFile == "" {
		return Settings{Mode: tlsx.ModeOff, Source: "plaintext"}, nil
	}

	// Legacy mode mirrors the previous behaviour exactly: one-way TLS on the
	// listeners and certificate verification (system roots or the configured
	// CA bundle) on outbound clients.
	server, err := tlsx.NewServerTLSConfig(tlsx.ServerConfig{
		CertFile: certFile,
		KeyFile:  strings.TrimSpace(legacy.KeyFile),
	})
	if err != nil {
		return Settings{}, fmt.Errorf("legacy tls server: %w", err)
	}
	client, err := LegacyClientConfig(legacy.ClientCAFile, legacy.InsecureSkipVerify)
	if err != nil {
		return Settings{}, err
	}
	return Settings{
		Mode:   tlsx.ModeOff,
		Server: server,
		Client: client,
		Source: "legacy --tls-cert-file/--tls-key-file",
	}, nil
}

// LegacyClientConfig builds the outbound TLS client configuration used before
// the STRATA_TLS_* contract existed. Certificates are verified against the
// system trust store by default; caFile optionally replaces it. Verification
// is only skipped when the caller explicitly opts in.
func LegacyClientConfig(caFile string, insecureSkipVerify bool) (*tls.Config, error) {
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
