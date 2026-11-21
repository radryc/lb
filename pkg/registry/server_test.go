package registry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rydzu/ainfra/lb/pkg/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
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

func TestHTTPServicesHandlerWithAuth(t *testing.T) {
	state := NewState(30000, 32767)
	const secret = "registry-secret-token"
	handler := HTTPServicesHandlerWithAuth(state, secret)

	// /healthz remains accessible without auth for probes.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz without auth = %d, want 200", rec.Code)
	}

	// /services and / reject missing or wrong bearer token.
	for _, path := range []string{"/services", "/"} {
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s without token = %d, want 401", path, rec.Code)
		}

		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer wrong-token")
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s with wrong token = %d, want 401", path, rec.Code)
		}

		req = httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s with valid token = %d, want 200", path, rec.Code)
		}
	}
}

func TestBearerAuthUnaryInterceptor(t *testing.T) {
	const secret = "grpc-secret-token"
	interceptor := BearerAuthUnaryInterceptor(secret)
	okHandler := func(ctx context.Context, req any) (any, error) {
		return "ok", nil
	}

	for _, method := range []string{
		"/lb.DiscoveryRegistry/RegisterService",
		"/lb.DiscoveryRegistry/DeregisterService",
	} {
		info := &grpc.UnaryServerInfo{FullMethod: method}

		// Missing metadata -> Unauthenticated.
		if _, err := interceptor(context.Background(), nil, info, okHandler); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("%s without metadata: code = %v, want Unauthenticated", method, status.Code(err))
		}

		// Wrong token -> Unauthenticated.
		badCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer wrong"))
		if _, err := interceptor(badCtx, nil, info, okHandler); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("%s with wrong token: code = %v, want Unauthenticated", method, status.Code(err))
		}

		// Valid token -> allowed.
		goodCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+secret))
		resp, err := interceptor(goodCtx, nil, info, okHandler)
		if err != nil || resp != "ok" {
			t.Fatalf("%s with valid token: resp=%v, err=%v", method, resp, err)
		}
	}

	// All RPCs require the token when one is configured, reads included.
	listInfo := &grpc.UnaryServerInfo{FullMethod: "/lb.DiscoveryRegistry/ListServices"}
	if _, err := interceptor(context.Background(), nil, listInfo, okHandler); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("ListServices without metadata: code = %v, want Unauthenticated", status.Code(err))
	}
	badListCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer wrong"))
	if _, err := interceptor(badListCtx, nil, listInfo, okHandler); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("ListServices with wrong token: code = %v, want Unauthenticated", status.Code(err))
	}
	goodListCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+secret))
	if resp, err := interceptor(goodListCtx, nil, listInfo, okHandler); err != nil || resp != "ok" {
		t.Fatalf("ListServices with valid token: resp=%v, err=%v", resp, err)
	}

	// Empty configured token disables enforcement.
	openInterceptor := BearerAuthUnaryInterceptor("")
	regInfo := &grpc.UnaryServerInfo{FullMethod: "/lb.DiscoveryRegistry/RegisterService"}
	if resp, err := openInterceptor(context.Background(), nil, regInfo, okHandler); err != nil || resp != "ok" {
		t.Fatalf("RegisterService with empty token config: resp=%v, err=%v", resp, err)
	}
}
