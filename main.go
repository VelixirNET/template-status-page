// A small, dependency-free status page.
//
// It polls a list of HTTP endpoints on a schedule, keeps a rolling window of results in
// memory, and serves a public page plus a JSON API.
//
// Configuration comes from monitors.json, or from the MONITORS environment variable when
// that is set. The env route is what makes this useful on a hosting platform: you can
// repoint the whole thing from the dashboard without editing code and redeploying.
//
// Deliberately stdlib-only. No database, no agent, no scraper. History lives in memory and
// resets on restart, which is the honest trade for something that builds in seconds and has
// no dependencies to keep patched. If you want history that survives a deploy, bind a
// managed Postgres and persist checks: that is a contained change to store.record plus a
// load on startup, and nothing else here has to move.
package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

// ─── Configuration ──────────────────────────────────────────────────────────

type Monitor struct {
	Name string `json:"name"`
	URL  string `json:"url"`

	// Defaults to GET.
	Method string `json:"method,omitempty"`

	// Status code that counts as healthy. Defaults to 200.
	ExpectStatus int `json:"expectStatus,omitempty"`

	// Defaults to 60, with a floor of 5: a mistyped interval should not turn this into a
	// load generator aimed at somebody else's service.
	IntervalSeconds int `json:"intervalSeconds,omitempty"`
}

type Config struct {
	Title    string    `json:"title"`
	Monitors []Monitor `json:"monitors"`
}

func loadConfig() (Config, error) {
	var raw []byte

	if env := os.Getenv("MONITORS"); env != "" {
		raw = []byte(env)
	} else {
		file, err := os.ReadFile("monitors.json")
		if err != nil {
			return Config{}, fmt.Errorf("no MONITORS env var, and monitors.json could not be read: %w", err)
		}
		raw = file
	}

	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("configuration is not valid JSON: %w", err)
	}
	if len(cfg.Monitors) == 0 {
		return Config{}, fmt.Errorf("configuration lists no monitors")
	}

	if cfg.Title == "" {
		cfg.Title = "Service status"
	}
	for i := range cfg.Monitors {
		m := &cfg.Monitors[i]
		if m.Method == "" {
			m.Method = http.MethodGet
		}
		if m.ExpectStatus == 0 {
			m.ExpectStatus = http.StatusOK
		}
		if m.IntervalSeconds < 5 {
			m.IntervalSeconds = 60
		}
		if m.Name == "" {
			m.Name = m.URL
		}
	}
	return cfg, nil
}

// ─── State ──────────────────────────────────────────────────────────────────

// historyLength is how many checks are kept per monitor. At a 60s interval that is an hour
// of detail, which is about what a status page actually gets read for.
const historyLength = 60

type Check struct {
	At        time.Time `json:"at"`
	OK        bool      `json:"ok"`
	Status    int       `json:"status,omitempty"`
	LatencyMs int64     `json:"latencyMs"`
	Error     string    `json:"error,omitempty"`
}

type store struct {
	mu      sync.RWMutex
	history map[string][]Check
}

func newStore() *store {
	return &store{history: make(map[string][]Check)}
}

func (s *store) record(name string, c Check) {
	s.mu.Lock()
	defer s.mu.Unlock()

	h := append(s.history[name], c)
	if len(h) > historyLength {
		h = h[len(h)-historyLength:]
	}
	s.history[name] = h
}

// snapshot returns a copy, so a reader can never race a writer appending to the same slice.
func (s *store) snapshot(name string) []Check {
	s.mu.RLock()
	defer s.mu.RUnlock()

	h := s.history[name]
	out := make([]Check, len(h))
	copy(out, h)
	return out
}

// ─── Checking ───────────────────────────────────────────────────────────────

func check(client *http.Client, m Monitor) Check {
	started := time.Now()

	req, err := http.NewRequest(m.Method, m.URL, nil)
	if err != nil {
		return Check{At: started, OK: false, Error: err.Error()}
	}
	req.Header.Set("User-Agent", "velixir-status-page")

	resp, err := client.Do(req)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		return Check{At: started, OK: false, LatencyMs: latency, Error: err.Error()}
	}
	defer resp.Body.Close()

	return Check{
		At:        started,
		OK:        resp.StatusCode == m.ExpectStatus,
		Status:    resp.StatusCode,
		LatencyMs: latency,
	}
}

// watch runs one monitor forever. It checks immediately rather than waiting a full interval,
// so a freshly deployed page has real data on the first page load instead of reading
// "no data yet" for a minute.
func watch(s *store, m Monitor) {
	client := &http.Client{Timeout: 10 * time.Second}

	s.record(m.Name, check(client, m))

	ticker := time.NewTicker(time.Duration(m.IntervalSeconds) * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.record(m.Name, check(client, m))
	}
}

// ─── Presentation ───────────────────────────────────────────────────────────

type barView struct {
	OK    bool
	Known bool
	Title string
}

type monitorView struct {
	Name      string
	URL       string
	Up        bool
	Known     bool
	UptimePct string
	LatencyMs int64
	LastAt    string
	LastError string
	Bars      []barView
}

type pageView struct {
	Title       string
	AllUp       bool
	AnyKnown    bool
	Monitors    []monitorView
	GeneratedAt string
}

func buildView(cfg Config, s *store) pageView {
	view := pageView{
		Title:       cfg.Title,
		AllUp:       true,
		GeneratedAt: time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
	}

	for _, m := range cfg.Monitors {
		h := s.snapshot(m.Name)
		mv := monitorView{Name: m.Name, URL: m.URL}

		if len(h) > 0 {
			view.AnyKnown = true
			mv.Known = true

			last := h[len(h)-1]
			mv.Up = last.OK
			mv.LatencyMs = last.LatencyMs
			mv.LastAt = last.At.UTC().Format("15:04:05 UTC")
			mv.LastError = last.Error

			up := 0
			for _, c := range h {
				if c.OK {
					up++
				}
			}
			mv.UptimePct = fmt.Sprintf("%.1f", float64(up)/float64(len(h))*100)

			if !last.OK {
				view.AllUp = false
			}
		}

		// Right-aligned: the newest check is the rightmost bar, and a page with little
		// history pads with unknowns so the strip does not resize as it fills up.
		for i := len(h); i < historyLength; i++ {
			mv.Bars = append(mv.Bars, barView{Known: false})
		}
		for _, c := range h {
			title := c.At.UTC().Format("15:04:05") + " UTC"
			switch {
			case c.OK:
				title += fmt.Sprintf(" - up (%dms)", c.LatencyMs)
			case c.Error != "":
				title += " - down: " + c.Error
			default:
				title += fmt.Sprintf(" - down (HTTP %d)", c.Status)
			}
			mv.Bars = append(mv.Bars, barView{OK: c.OK, Known: true, Title: title})
		}

		view.Monitors = append(view.Monitors, mv)
	}

	sort.SliceStable(view.Monitors, func(i, j int) bool {
		return view.Monitors[i].Name < view.Monitors[j].Name
	})
	return view
}

// html/template escapes every interpolated value, which matters here: monitor names and
// error strings are configuration and remote output, not trusted markup.
var page = template.Must(template.New("status").Parse(`<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title>
<meta http-equiv="refresh" content="30">
<style>
  :root { color-scheme: dark; }
  * { box-sizing: border-box; }
  body { margin:0; min-height:100vh; background:#0b0b10; color:#e8e8ef;
         font:15px/1.6 ui-sans-serif,system-ui,-apple-system,Segoe UI,sans-serif; }
  .wrap { max-width:46rem; margin:0 auto; padding:3rem 1.25rem 4rem; }
  h1 { font-size:1.5rem; margin:0 0 .25rem; letter-spacing:-.02em; }
  .sub { color:#7a7a8c; font-size:.8125rem; margin:0 0 2rem; }
  .banner { display:flex; align-items:center; gap:.625rem; padding:1rem 1.25rem;
            border-radius:10px; margin-bottom:2rem; font-weight:600; }
  .banner.up { background:rgba(52,211,153,.1); border:1px solid rgba(52,211,153,.25); color:#6ee7b7; }
  .banner.down { background:rgba(248,113,113,.1); border:1px solid rgba(248,113,113,.25); color:#fca5a5; }
  .banner.unknown { background:rgba(148,163,184,.1); border:1px solid rgba(148,163,184,.25); color:#cbd5e1; }
  .dot { width:.5rem; height:.5rem; border-radius:50%; background:currentColor; flex:none; }
  .card { border:1px solid #1f1f2e; border-radius:10px; padding:1.125rem 1.25rem; margin-bottom:.75rem;
          background:linear-gradient(180deg,rgba(255,255,255,.015),transparent 60%); }
  .row { display:flex; align-items:baseline; justify-content:space-between; gap:1rem; }
  .name { font-weight:600; }
  .meta { color:#7a7a8c; font-size:.8125rem; font-variant-numeric:tabular-nums; }
  .bars { display:flex; gap:2px; margin-top:.875rem; height:1.75rem; align-items:stretch; }
  .bar { flex:1 1 0; min-width:2px; border-radius:2px; }
  .bar.ok { background:#34d399; }
  .bar.bad { background:#f87171; }
  .bar.unknown { background:#1f1f2e; }
  .err { color:#fca5a5; font-size:.8125rem; margin-top:.5rem; word-break:break-word; }
  footer { color:#4b4b5c; font-size:.75rem; margin-top:2.5rem; text-align:center; }
  footer a { color:#7a7a8c; }
</style>
<div class="wrap">
  <h1>{{.Title}}</h1>
  <p class="sub">Updated {{.GeneratedAt}}. This page refreshes itself every 30 seconds.</p>

  {{if not .AnyKnown}}
    <div class="banner unknown"><span class="dot"></span>Waiting for the first checks</div>
  {{else if .AllUp}}
    <div class="banner up"><span class="dot"></span>All systems operational</div>
  {{else}}
    <div class="banner down"><span class="dot"></span>Some systems are down</div>
  {{end}}

  {{range .Monitors}}
    <div class="card">
      <div class="row">
        <span class="name">{{.Name}}</span>
        <span class="meta">
          {{if .Known}}{{.UptimePct}}% &middot; {{.LatencyMs}}ms &middot; {{.LastAt}}{{else}}no data yet{{end}}
        </span>
      </div>
      <div class="bars">
        {{range .Bars}}<div class="bar {{if not .Known}}unknown{{else if .OK}}ok{{else}}bad{{end}}" title="{{.Title}}"></div>{{end}}
      </div>
      {{if .LastError}}<div class="err">{{.LastError}}</div>{{end}}
    </div>
  {{end}}

  <footer>
    <a href="/api/status">JSON API</a> &middot; running on <a href="https://velixir.net">velixir</a>
  </footer>
</div>
</html>`))

// ─── Entry point ────────────────────────────────────────────────────────────

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}

	s := newStore()
	for _, m := range cfg.Monitors {
		go watch(s, m)
	}

	mux := http.NewServeMux()

	// Liveness only. This reports that the status page itself is up, never whether the
	// things it monitors are: returning 503 during somebody else's outage would make the
	// platform restart the status page at exactly the moment people are reading it.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		out := make(map[string][]Check, len(cfg.Monitors))
		for _, m := range cfg.Monitors {
			out[m.Name] = s.snapshot(m.Name)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(out); err != nil {
			log.Printf("encoding status: %v", err)
		}
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// ServeMux routes "/" as a catch-all, so anything unmatched lands here.
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := page.Execute(w, buildView(cfg, s)); err != nil {
			log.Printf("rendering page: %v", err)
		}
	})

	// velixir injects PORT. Bind every interface so the health check can reach us.
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("status page listening on %s, watching %d monitor(s)", port, len(cfg.Monitors))
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
