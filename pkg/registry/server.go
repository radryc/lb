package registry

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/rydzu/ainfra/lb/pkg/pb"
)

//go:embed design/tokens.css
var tokensCSS string

//go:embed design/ui.css
var uiCSS string

//go:embed fonts/*.woff2
var fontFiles embed.FS

type GRPCServer struct {
	pb.UnimplementedDiscoveryRegistryServer
	state *State
}

func NewGRPCServer(state *State) *GRPCServer {
	return &GRPCServer{state: state}
}

func (s *GRPCServer) RegisterService(ctx context.Context, req *pb.RegisterServiceRequest) (*pb.RegisterServiceResponse, error) {
	externalPort, err := s.state.Register(req)
	if err != nil {
		return nil, err
	}
	return &pb.RegisterServiceResponse{ExternalPort: externalPort}, nil
}

func (s *GRPCServer) DeregisterService(ctx context.Context, req *pb.DeregisterServiceRequest) (*pb.DeregisterServiceResponse, error) {
	removed, err := s.state.Deregister(req)
	if err != nil {
		return nil, err
	}
	return &pb.DeregisterServiceResponse{Removed: removed}, nil
}

func (s *GRPCServer) ListServices(ctx context.Context, req *pb.ListServicesRequest) (*pb.ListServicesResponse, error) {
	return s.state.ListResponse(), nil
}

func HTTPServicesHandler(state *State) http.Handler {
	mux := http.NewServeMux()

	fonts, err := fs.Sub(fontFiles, "fonts")
	if err != nil {
		panic(fmt.Sprintf("registry: embedded fonts unavailable: %v", err))
	}
	mux.Handle("/fonts/", http.StripPrefix("/fonts/", http.FileServer(http.FS(fonts))))

	mux.HandleFunc("/services", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(state.Snapshot()); err != nil {
			log.Printf("encode /services response: %v", err)
		}
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/ui" {
			http.NotFound(w, r)
			return
		}
		snap := state.Snapshot()

		// Group services by account, sort for stable output.
		byAccount := map[string][]serviceUI{}
		for _, svc := range snap.Services {
			acc := svc.Account
			if acc == "" {
				acc = "default"
			}
			proto := strings.ToLower(svc.Protocol)
			scheme := "http"
			if proto == "grpc" {
				scheme = "grpc"
			}
			// Build the host from the request so the link works from any machine.
			host := r.Host
			if h, _, err := splitHostPort(host); err == nil {
				host = fmt.Sprintf("%s:%d", h, svc.ExternalPort)
			} else {
				host = fmt.Sprintf("%s:%d", host, svc.ExternalPort)
			}
			url := fmt.Sprintf("%s://%s", scheme, host)
			byAccount[acc] = append(byAccount[acc], serviceUI{
				Name:        svc.ServiceName,
				Port:        svc.ExternalPort,
				Protocol:    svc.Protocol,
				Description: svc.Description,
				URL:         url,
			})
		}
		accounts := make([]string, 0, len(byAccount))
		for a := range byAccount {
			accounts = append(accounts, a)
		}
		sort.Strings(accounts)
		groups := make([]uiGroup, 0, len(accounts))
		for _, a := range accounts {
			svcs := byAccount[a]
			sort.Slice(svcs, func(i, j int) bool { return svcs[i].Port < svcs[j].Port })
			groups = append(groups, uiGroup{Account: a, Services: svcs})
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := renderUI(w, groups); err != nil {
			log.Printf("render ui: %v", err)
		}
	})
	return mux
}

type serviceUI struct {
	Name        string
	Port        int32
	Protocol    string
	Description string
	URL         string
}

func splitHostPort(hostport string) (host string, port string, err error) {
	// net.SplitHostPort wrapper used to avoid importing net in this file.
	i := strings.LastIndex(hostport, ":")
	if i < 0 {
		return hostport, "", fmt.Errorf("no port")
	}
	return hostport[:i], hostport[i+1:], nil
}

type uiGroup struct {
	Account  string
	Services []serviceUI
}

const uiHead = `<!DOCTYPE html>
<html lang="en" data-theme="midnight">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>LB Edge — Services</title>
<script>
(function () {
  try {
    var THEME_KEY = "strata.theme";
    var LEGACY_THEME_KEY = "lb.theme";
    var theme = localStorage.getItem(THEME_KEY) || localStorage.getItem(LEGACY_THEME_KEY);
    if (!theme) theme = "system";
    var resolved = theme;
    if (theme === "system") {
      resolved = window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches
        ? "daylight"
        : "midnight";
    }
    document.documentElement.setAttribute("data-theme", resolved);
    document.documentElement.dataset.themePreference = theme;
  } catch (e) { /* localStorage unavailable */ }
})();
</script>
<style>`

const uiLocalCSS = `@font-face {
  font-family: "Manrope";
  src: url("/fonts/manrope-latin.woff2") format("woff2");
  font-style: normal;
  font-weight: 400 800;
  font-display: swap;
}
@font-face {
  font-family: "Space Grotesk";
  src: url("/fonts/space-grotesk-latin.woff2") format("woff2");
  font-style: normal;
  font-weight: 500 700;
  font-display: swap;
}
@font-face {
  font-family: "JetBrains Mono";
  src: url("/fonts/jetbrains-mono-latin.woff2") format("woff2");
  font-style: normal;
  font-weight: 400 600;
  font-display: swap;
}
html, body { min-height: 100%; }
body {
  margin: 0;
  background: var(--strata-bg);
  color: var(--strata-text);
  font-family: var(--strata-font-ui);
  font-size: var(--strata-text-base);
  line-height: 1.5;
  font-variant-numeric: tabular-nums;
}
a { color: inherit; }
.s-app { min-height: 100vh; }
.lb-brand {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  color: var(--strata-text);
  font-family: var(--strata-font-display);
  font-weight: 700;
  text-decoration: none;
}
.lb-brand__dot {
  width: 9px;
  height: 9px;
  border-radius: 50%;
  background: var(--strata-accent);
  box-shadow: 0 0 0 3px var(--strata-overlay-soft);
}
.theme-picker {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 28px;
  padding: 0 4px 0 8px;
  border: 1px solid var(--strata-border);
  border-radius: 999px;
  color: var(--strata-text-secondary);
  background: var(--strata-elevated);
  cursor: pointer;
}
.theme-picker:hover { border-color: var(--strata-border-strong); }
.theme-picker-swatch {
  width: 10px;
  height: 10px;
  flex: 0 0 auto;
  border-radius: 50%;
  background: var(--strata-accent);
  box-shadow: 0 0 0 1px var(--strata-border-strong) inset;
}
.theme-picker select {
  min-height: 0;
  padding: 0 2px;
  border: 0;
  outline: none;
  background: transparent;
  color: var(--strata-text-secondary);
  font-size: 11px;
  font-weight: 600;
  cursor: pointer;
}
.theme-picker select option { background: var(--strata-panel); color: var(--strata-text); }
.lb-content { max-width: 1200px; }
.lb-group { margin-top: 28px; }
.lb-group__title {
  margin: 0 0 10px;
  color: var(--strata-text-tertiary);
  font-size: 11px;
  font-weight: 800;
  letter-spacing: 0.12em;
  text-transform: uppercase;
}
.lb-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(260px, 1fr));
  gap: 12px;
}
.lb-card {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 14px 16px;
  text-decoration: none;
  transition: border-color var(--strata-fast) var(--strata-ease),
    background var(--strata-fast) var(--strata-ease);
}
.lb-card:hover {
  border-color: var(--strata-accent);
  background: var(--strata-elevated);
}
.lb-card__top {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  min-width: 0;
}
.lb-card__name {
  overflow: hidden;
  color: var(--strata-text);
  font-weight: 700;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.lb-card__port {
  color: var(--strata-text-secondary);
  font-family: var(--strata-font-mono);
  font-size: 12px;
}
.lb-card__desc {
  overflow: hidden;
  color: var(--strata-text-tertiary);
  font-size: 12px;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.lb-badge--http {
  color: var(--strata-accent);
  border-color: color-mix(in oklab, var(--strata-accent) 32%, transparent);
  background: color-mix(in oklab, var(--strata-accent) 10%, transparent);
}
.lb-badge--grpc {
  color: var(--strata-ai);
  border-color: color-mix(in oklab, var(--strata-ai) 32%, transparent);
  background: color-mix(in oklab, var(--strata-ai) 10%, transparent);
}
.lb-badge--tcp { color: var(--strata-text-secondary); }
@media (max-width: 640px) {
  .s-topbar { padding: 0 12px; }
  .s-content { padding: var(--strata-space-4); }
  .lb-grid { grid-template-columns: 1fr; }
}
`

const uiBodyStart = `</style>
</head>
<body>
<a class="s-skip-link" href="#main-content">Skip to content</a>
<div class="s-app">
<main class="s-main" id="main-content">
<header class="s-topbar">
  <div class="s-topbar__crumb">
    <a class="lb-brand" href="/"><span class="lb-brand__dot" aria-hidden="true"></span><span>LB Edge</span></a>
    <span class="s-topbar__sep">/</span>
    <strong class="s-topbar__title">Services</strong>
  </div>
  <div class="s-topbar__spacer"></div>
  <div class="s-topbar__actions">
    <label class="theme-picker" title="Color theme">
      <span class="theme-picker-swatch" aria-hidden="true"></span>
      <select id="themeSelect" aria-label="Color theme">
        <option value="system">System</option>
        <option value="midnight">Midnight</option>
        <option value="daylight">Daylight</option>
        <option value="graphite">Graphite</option>
        <option value="ocean">Ocean</option>
        <option value="forest">Forest</option>
      </select>
    </label>
  </div>
</header>
<div class="s-content lb-content">
`

const uiTail = `</div>
</main>
</div>
<script>
(function () {
  var THEME_KEY = "strata.theme";
  var LEGACY_THEME_KEY = "lb.theme";
  var THEMES = ["system", "midnight", "daylight", "graphite", "ocean", "forest"];

  function isTheme(value) {
    return THEMES.indexOf(value) !== -1;
  }

  function resolveTheme(theme) {
    if (theme !== "system") return isTheme(theme) ? theme : "midnight";
    try {
      return window.matchMedia("(prefers-color-scheme: light)").matches ? "daylight" : "midnight";
    } catch (e) {
      return "midnight";
    }
  }

  function readStoredTheme() {
    try {
      var stored = localStorage.getItem(THEME_KEY);
      if (isTheme(stored)) return stored;
      var legacy = localStorage.getItem(LEGACY_THEME_KEY);
      if (isTheme(legacy) && legacy !== "system") return legacy;
    } catch (e) { /* localStorage unavailable */ }
    return "system";
  }

  function applyTheme(theme, persist) {
    var next = isTheme(theme) ? theme : "system";
    var resolved = resolveTheme(next);
    document.documentElement.setAttribute("data-theme", resolved);
    document.documentElement.dataset.themePreference = next;
    if (persist) {
      try { localStorage.setItem(THEME_KEY, next); } catch (e) { /* ignore */ }
    }
  }

  var select = document.getElementById("themeSelect");
  var saved = readStoredTheme();
  applyTheme(saved, false);
  if (select) {
    select.value = saved;
    select.addEventListener("change", function () {
      applyTheme(select.value, true);
    });
  }

  try {
    var media = window.matchMedia("(prefers-color-scheme: light)");
    if (media.addEventListener) {
      media.addEventListener("change", function () {
        if (readStoredTheme() === "system") applyTheme("system", false);
      });
    }
  } catch (e) { /* matchMedia unavailable */ }
})();
</script>
</body>
</html>`

func renderUI(w http.ResponseWriter, groups []uiGroup) error {
	total := 0
	for _, g := range groups {
		total += len(g.Services)
	}

	if _, err := fmt.Fprint(w, uiHead, tokensCSS, "</style>\n<style>\n", uiCSS, "</style>\n<style>\n", uiLocalCSS, uiBodyStart); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(w, `<div class="s-page-header">
  <div class="s-page-header__copy">
    <p class="s-eyebrow">Edge registry</p>
    <h1 class="s-title">Services</h1>
    <p class="s-subtitle">L4 routes currently registered with LB Edge.</p>
    <div class="s-meta"><span>%d services</span><span>·</span><span>%d accounts</span></div>
  </div>
  <div class="s-actions">
    <button class="s-btn s-btn-secondary" type="button" onclick="location.reload()">Refresh</button>
  </div>
</div>
`, total, len(groups)); err != nil {
		return err
	}

	if len(groups) == 0 {
		if _, err := fmt.Fprint(w, `<div class="s-empty"><div class="s-empty__title">No services registered yet</div><div class="s-empty__text">Annotate a Kubernetes Service with <code>guardian.intent/expose=true</code> to register it with LB Edge.</div></div>`); err != nil {
			return err
		}
	} else {
		for _, g := range groups {
			if _, err := fmt.Fprintf(w, `<section class="lb-group"><h2 class="lb-group__title">%s</h2><div class="lb-grid">`, htmlEscape(g.Account)); err != nil {
				return err
			}
			for _, s := range g.Services {
				proto := strings.ToLower(s.Protocol)
				if proto == "" {
					proto = "tcp"
				}
				badgeClass := "lb-badge--tcp"
				switch proto {
				case "http":
					badgeClass = "lb-badge--http"
				case "grpc":
					badgeClass = "lb-badge--grpc"
				}
				desc := s.Description
				if desc == "" {
					desc = s.Name
				}
				target := s.URL
				if proto == "grpc" {
					target = "#" // grpc can't be opened in a browser directly
				}
				if _, err := fmt.Fprintf(w,
					`<a class="s-card lb-card" href="%s" target="_blank" rel="noopener">`+
						`<div class="lb-card__top">`+
						`<span class="lb-card__name">%s</span>`+
						`<span class="s-badge %s">%s</span>`+
						`</div>`+
						`<span class="lb-card__port">:%d</span>`+
						`<span class="lb-card__desc">%s</span>`+
						`</a>`+"\n",
					htmlEscape(target),
					htmlEscape(s.Name),
					badgeClass,
					htmlEscape(strings.ToUpper(proto)),
					s.Port,
					htmlEscape(desc),
				); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprint(w, "</div></section>\n"); err != nil {
				return err
			}
		}
	}

	_, err := fmt.Fprint(w, uiTail)
	return err
}

func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&#34;")
	return s
}
