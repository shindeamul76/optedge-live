package dashboard

import (
	"context"
	"encoding/json"
	_ "embed"
	"net/http"
	"time"
)

//go:embed index.html
var indexHTML []byte

// Server serves the embedded dashboard page and the /api/state snapshot. The snapshot
// is produced by the injected provider on each request, so it always reflects current
// live + persisted state.
type Server struct {
	provider func() Snapshot
}

// NewServer builds a dashboard server over a snapshot provider.
func NewServer(provider func() Snapshot) *Server { return &Server{provider: provider} }

// Handler returns the HTTP routes (also handy for tests via httptest).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(s.provider())
	})
	return mux
}

// Start runs the server until ctx is cancelled, then shuts it down gracefully.
func (s *Server) Start(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler()}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
