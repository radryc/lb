package proxy

import (
	"crypto/tls"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/radryc/tlsx"
	"github.com/radryc/tlsx/pki"
)

// generateProxyPKI issues a CA plus the proxy and backend identities used by
// the data-plane TLS tests.
func generateProxyPKI(t *testing.T) *pki.Bundle {
	t.Helper()
	bundle, err := pki.Generate(pki.Options{
		Dir:         t.TempDir(),
		TrustDomain: "strata.test",
		Validity:    time.Hour,
		Services: []pki.Service{
			{Name: "proxy", Namespace: "lb", IPs: []string{"127.0.0.1"}},
			{Name: "backend", Namespace: "lb", IPs: []string{"127.0.0.1"}},
		},
	})
	if err != nil {
		t.Fatalf("pki.Generate: %v", err)
	}
	return bundle
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	_ = lis.Close()
	return port
}

func startTLSEchoServer(t *testing.T, tlsCfg *tls.Config) (addr string, port int) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	tlsLis := tls.NewListener(lis, tlsCfg)
	t.Cleanup(func() { _ = tlsLis.Close() })
	go func() {
		for {
			conn, err := tlsLis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	return lis.Addr().String(), lis.Addr().(*net.TCPAddr).Port
}

// newTLSProxyEngine builds an engine whose data plane terminates mTLS and
// dials backends over mTLS, then installs a single backend.
func newTLSProxyEngine(t *testing.T, bundle *pki.Bundle) (port int, cleanup func()) {
	t.Helper()
	proxyIdentity := bundle.Services["proxy"]
	backendIdentity := bundle.Services["backend"]

	serverCfg, err := tlsx.NewServerTLSConfig(tlsx.ServerConfig{
		CertFile:          proxyIdentity.CertFile,
		KeyFile:           proxyIdentity.KeyFile,
		CAFile:            bundle.CAFile,
		RequireClientCert: true,
	})
	if err != nil {
		t.Fatalf("proxy server TLS: %v", err)
	}
	backendServerCfg, err := tlsx.NewServerTLSConfig(tlsx.ServerConfig{
		CertFile:          backendIdentity.CertFile,
		KeyFile:           backendIdentity.KeyFile,
		CAFile:            bundle.CAFile,
		RequireClientCert: true,
	})
	if err != nil {
		t.Fatalf("backend server TLS: %v", err)
	}
	clientCfg, err := tlsx.NewClientTLSConfig(tlsx.ClientConfig{
		CertFile:   proxyIdentity.CertFile,
		KeyFile:    proxyIdentity.KeyFile,
		CAFile:     bundle.CAFile,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatalf("proxy client TLS: %v", err)
	}

	_, backendPort := startTLSEchoServer(t, backendServerCfg)

	engine := NewEngine("127.0.0.1", "http://127.0.0.1:1", time.Second, time.Second, nil, false)
	engine.SetDataPlaneTLS(serverCfg, clientCfg)

	externalPort := freeTCPPort(t)
	li, err := engine.startListenerLocked(int32(externalPort))
	if err != nil {
		t.Fatalf("start listener: %v", err)
	}
	li.mu.Lock()
	li.backends[backendMapKey("127.0.0.1", int32(backendPort))] = newBackend("127.0.0.1", int32(backendPort))
	li.mu.Unlock()

	return externalPort, func() { _ = li.listener.Close() }
}

func proxyTLSClient(t *testing.T, bundle *pki.Bundle) *tls.Config {
	t.Helper()
	identity := bundle.Services["proxy"]
	cfg, err := tlsx.NewClientTLSConfig(tlsx.ClientConfig{
		CertFile:   identity.CertFile,
		KeyFile:    identity.KeyFile,
		CAFile:     bundle.CAFile,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatalf("client TLS config: %v", err)
	}
	return cfg
}

func TestProxyDataPlaneTLSHandshake(t *testing.T) {
	bundle := generateProxyPKI(t)
	externalPort, cleanup := newTLSProxyEngine(t, bundle)
	defer cleanup()

	conn, err := tls.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(externalPort)), proxyTLSClient(t, bundle))
	if err != nil {
		t.Fatalf("TLS dial proxy: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	payload := []byte("hello-over-mtls")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, echo); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(echo) != string(payload) {
		t.Fatalf("echo = %q, want %q", echo, payload)
	}
}

func TestProxyDataPlaneRejectsPlaintext(t *testing.T) {
	bundle := generateProxyPKI(t)
	externalPort, cleanup := newTLSProxyEngine(t, bundle)
	defer cleanup()

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(externalPort)), 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: proxy\r\n\r\n")); err != nil {
		// A reset is also a rejection.
		return
	}
	buf := make([]byte, 256)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("plaintext connection must not be proxied on a TLS data plane")
	}
}

func TestProxyDataPlaneDefaultsToPlaintext(t *testing.T) {
	backendLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen backend: %v", err)
	}
	defer backendLis.Close()
	go func() {
		for {
			conn, err := backendLis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	backendPort := backendLis.Addr().(*net.TCPAddr).Port

	engine := NewEngine("127.0.0.1", "http://127.0.0.1:1", time.Second, time.Second, nil, false)
	port := freeTCPPort(t)
	li, err := engine.startListenerLocked(int32(port))
	if err != nil {
		t.Fatalf("start listener: %v", err)
	}
	defer li.listener.Close()
	li.mu.Lock()
	li.backends[backendMapKey("127.0.0.1", int32(backendPort))] = newBackend("127.0.0.1", int32(backendPort))
	li.mu.Unlock()

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	payload := []byte("plaintext-default")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, echo); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(echo) != string(payload) {
		t.Fatalf("echo = %q, want %q", echo, payload)
	}
}
