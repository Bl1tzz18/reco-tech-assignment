package obs

import (
	"encoding/json"
	"expvar"
	"net/http"
	"net/http/pprof"
)

// Handler serves the observability endpoints:
//
//	GET /healthz       200 when every job succeeded recently, else 503 (JSON body per job)
//	GET /metrics       this process's Metrics as JSON
//	GET /debug/vars    expvar: Metrics (once published) + Go runtime memstats
//	GET /debug/pprof/  CPU / heap / goroutine profiles
//
// pprof exposes internals, so bind the server to localhost or a private network.
func Handler(m *Metrics) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		jobs, healthy := m.Health()
		body := struct {
			Status string            `json:"status"`
			Jobs   map[string]string `json:"jobs"`
		}{Status: "ok", Jobs: jobs}
		code := http.StatusOK
		if !healthy {
			body.Status, code = "unhealthy", http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body) // the client hung up; nothing useful to do
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(m.String()))
	})
	mux.Handle("GET /debug/vars", expvar.Handler())
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	return mux
}
