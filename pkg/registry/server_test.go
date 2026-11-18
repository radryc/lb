package registry

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rydzu/ainfra/lb/pkg/pb"
)

func TestServicesUIUsesStrataTheme(t *testing.T) {
	state := NewState(30000, 32767)
	if _, err := state.Register(&pb.RegisterServiceRequest{
		ServiceName:           "prometheus-edge",
		TargetIp:              "10.0.0.1",
		TargetPort:            9090,
		RequestedExternalPort: 9091,
	}); err != nil {
		t.Fatalf("register service: %v", err)
	}

	srv := httptest.NewServer(HTTPServicesHandler(state))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	html := string(body)
	for _, want := range []string{
		`<option value="system">System</option>`,
		`strata.theme`,
		`prefers-color-scheme: light`,
		`s-topbar`,
		`theme-picker`,
		`prometheus-edge`,
		`:9091`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("UI is missing %q", want)
		}
	}
}

func TestServicesUIServesEmbeddedFonts(t *testing.T) {
	srv := httptest.NewServer(HTTPServicesHandler(NewState(30000, 32767)))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/fonts/manrope-latin.woff2")
	if err != nil {
		t.Fatalf("GET font: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET font status = %d, want 200", resp.StatusCode)
	}
	prefix := make([]byte, 4)
	if _, err := io.ReadFull(resp.Body, prefix); err != nil {
		t.Fatalf("read font prefix: %v", err)
	}
	if string(prefix) != "wOF2" {
		t.Fatalf("font prefix = %q, want wOF2", prefix)
	}
}

func TestServicesUISanitizesHostHeader(t *testing.T) {
	state := NewState(30000, 32767)
	if _, err := state.Register(&pb.RegisterServiceRequest{
		ServiceName:           "web-app",
		TargetIp:              "10.0.0.1",
		TargetPort:            8080,
		RequestedExternalPort: 9091,
	}); err != nil {
		t.Fatalf("register service: %v", err)
	}

	handler := HTTPServicesHandler(state)

	cases := []struct {
		name     string
		host     string
		wantHref string
	}{
		{"normal host with port", "edge.internal:8081", `href="http://edge.internal:9091"`},
		{"normal host without port", "edge.internal", `href="http://edge.internal:9091"`},
		{"ipv6 with port", "[::1]:8081", `href="http://[::1]:9091"`},
		{"crlf injection", "evil.com\r\nX-Injected: 1", `href="http://localhost:9091"`},
		{"xss injection", `evil.com"><script>alert(1)</script>`, `href="http://localhost:9091"`},
		{"path injection", "evil.com/phish?x=1#y", `href="http://localhost:9091"`},
		{"userinfo injection", "user@evil.com", `href="http://localhost:9091"`},
		{"empty host", "", `href="http://localhost:9091"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Host = tc.host
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, tc.wantHref) {
				t.Errorf("response missing %q; body:\n%s", tc.wantHref, body)
			}
			if strings.Contains(body, "evil.com") {
				t.Errorf("response leaked unsanitized host in body:\n%s", body)
			}
		})
	}
}
