package sandbox

import (
	"io"
	"net"
	"net/http"
	"time"
)

// ListenHook is the local routine the sandbox's agent test posts to. Any POST is accepted,
// so the panel can show that the routine took the test. It is not a Grok Bot routine.
func ListenHook(addr string) (*http.Server, string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
		w.WriteHeader(http.StatusNoContent)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	return srv, "http://" + ln.Addr().String() + "/hook", nil
}
