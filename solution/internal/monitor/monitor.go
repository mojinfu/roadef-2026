// Package monitor serves a local live-view page for the solve loop.
//
// It binds a tiny HTTP server on 127.0.0.1 (default port 8765, matching the
// V1.0 design's -liveview-port; 0 asks for a free port) exposing
//
//	/api/status -> JSON of the latest solve snapshot (the page polls it),
//	/           -> a self-contained HTML page that redraws every ~0.6 s.
//
// The solve loop calls Server.Publish once per round, before the expensive
// pool build / MIP, so the "currently solving" hot cells stay on screen while
// Gurobi runs, and once more when solving stops.  OpenBrowser is separate so
// callers can choose when to pop the tab (and opt out via an env var).
package monitor

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"
)

//go:embed static
var staticFS embed.FS

// Server keeps the latest dashboard snapshot and serves it over HTTP.
type Server struct {
	mu     sync.RWMutex
	latest *State

	// baseMS anchors the elapsed counter at the wall-clock instant the solve
	// started (set on each Publish from that snapshot's ElapsedMS), so LiveTick
	// can advance the served clock even while a long MIP round runs.
	baseMS int64

	srv *http.Server
	ln  net.Listener
}

// New returns a Server.  topN is the default bars-per-slot fallback used by the
// page when a published state does not carry BarTop (it should always carry it).
func New() *Server {
	return &Server{}
}

// Listen binds 127.0.0.1:port and starts serving in the background.  When port
// is 0 an OS-assigned free port is used; when the requested port is busy the
// server falls back to a free one so parallel instances do not kill the view.
// It returns the page URL.
func (s *Server) Listen(port int) (string, error) {
	addr := "127.0.0.1:0"
	if port > 0 {
		addr = fmt.Sprintf("127.0.0.1:%d", port)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if port > 0 {
			// Requested port taken (e.g. a previous instance still lingering) —
			// fall back to a free port instead of disabling the view.
			ln, err = net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return "", err
			}
		} else {
			return "", err
		}
	}
	s.ln = ln
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/", s.handleRoot)
	s.srv = &http.Server{Handler: mux}
	go s.srv.Serve(ln) //nolint:errcheck // best-effort; failures surface on fetch
	return fmt.Sprintf("http://127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port), nil
}

// Publish atomically replaces the served snapshot.
func (s *Server) Publish(st *State) {
	if st == nil {
		return
	}
	now := time.Now().UnixMilli()
	st.ServerNowMS = now
	s.mu.Lock()
	s.latest = st
	s.baseMS = now - st.ElapsedMS
	s.mu.Unlock()
}

// LiveTick is a cheap 1 Hz heartbeat: it copies the served snapshot under the
// lock and re-stamps elapsed / server-now so the page clock stays live while a
// round's pool build + Gurobi call is still running (real data only lands when
// the next round publishes).  It is a no-op once solving is done, freezing the
// final elapsed reading during the linger window.
func (s *Server) LiveTick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.latest == nil || s.latest.Status == "done" {
		return
	}
	cp := *s.latest
	now := time.Now().UnixMilli()
	cp.ElapsedMS = now - s.baseMS
	cp.ServerNowMS = now
	s.latest = &cp
}

// Close stops the HTTP server.
func (s *Server) Close() {
	if s.srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.srv.Shutdown(ctx) //nolint:errcheck
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	st := s.latest
	s.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	if st == nil {
		w.Write([]byte(`{"status":"idle"}`)) //nolint:errcheck
		return
	}
	b, err := json.Marshal(st)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Write(b) //nolint:errcheck
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "monitor page missing", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b) //nolint:errcheck
}

// OpenBrowser opens url in the default browser.  It is best-effort: on failure
// the URL has already been printed by the caller, so the user can click it.
// Setting TASR_MONITOR_NO_OPEN=1 (used by bench/CI runs) disables it.
func OpenBrowser(url string) {
	if os.Getenv("TASR_MONITOR_NO_OPEN") == "1" {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if cmd == nil {
		return
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "monitor: open browser: %v\n", err)
	}
}
