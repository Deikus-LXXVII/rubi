package sandbox

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"strings"
	"time"
)

// SignInURL is a long https address on the sandbox OAuth stub. It is longer than the 400 runes the
// T3 Agents page keeps of a link, so a phone can only use it if the panel shows the whole thing.
func SignInURL(port string) string {
	return "https://127.0.0.1:" + port + "/oauth/authorize?response_type=code&client_id=sandbox&state=" + strings.Repeat("a", 480)
}

// ListenOAuth serves the fake Claude sign-in page over TLS on addr. The certificate is generated
// for this process and is not a real Claude certificate.
func ListenOAuth(addr, certFile, keyFile string) (*http.Server, string, error) {
	if err := writeCert(certFile, keyFile); err != nil {
		return nil, "", err
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, "", err
	}
	ln, err := tls.Listen("tcp", addr, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		return nil, "", err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(`<!doctype html><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Claude sign-in (sandbox)</title>
<main style="font:18px/1.4 sans-serif;padding:24px;max-width:36rem">
<h1>Claude sign-in</h1>
<p>This is the sandbox sign-in page, not Claude. Copy the code below into Rubi.</p>
<p id="code" style="font:28px/1.2 ui-monospace,monospace;user-select:all">` + SignInCode + `</p>
</main>`))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return srv, port, nil
}

func writeCert(certFile, keyFile string) error {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "rubi-sandbox"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	if err := writePEM(certFile, "CERTIFICATE", der); err != nil {
		return err
	}
	return writePEM(keyFile, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key))
}

func writePEM(path, kind string, der []byte) error {
	return writeFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}))
}
