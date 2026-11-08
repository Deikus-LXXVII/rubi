package agentbridge

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"strconv"
	"time"
)

// Frontend is only needed when an unmodified host application (T3/OpenCode)
// requires a TCP URL. Project T3 clients use Process directly and bind no host port.
func Frontend(p *Process, port int, password string) (*http.Server, error) {
	if len(password) < 32 {
		return nil, errors.New("bridge: OpenCode needs a nonempty server password of at least 32 characters")
	}
	expected := sha256.Sum256([]byte(password))
	proxy := &httputil.ReverseProxy{Transport: p, FlushInterval: -1, ErrorLog: log.New(io.Discard, "", 0),
		Director: func(r *http.Request) {
			r.URL.Scheme = "http"
			r.URL.Host = "127.0.0.1:" + strconv.Itoa(port)
			r.Host = r.URL.Host
			r.Header.Del("Proxy-Authorization")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "sandboxed service unavailable", http.StatusBadGateway)
		},
	}
	return &http.Server{ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 1 << 20, ErrorLog: log.New(io.Discard, "", 0), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		got := sha256.Sum256([]byte(pass))
		if !ok || user != "opencode" || subtle.ConstantTimeCompare(got[:], expected[:]) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodConnect {
			http.Error(w, "unsupported method", http.StatusMethodNotAllowed)
			return
		}
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			http.Error(w, "duplex transport unavailable", http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	})}, nil
}
