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
