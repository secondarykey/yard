package server

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yard/internal/config"
)

// writeCert writes a new self-signed certificate and its key as dir/yard.crt and dir/yard.key
// and returns the certificate (DER).
func writeCert(t *testing.T, dir string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "yard test"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, typ string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b}), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("yard.crt", "CERTIFICATE", der)
	write("yard.key", "PRIVATE KEY", keyDER)
	return der
}

// TestTLS serves yard over HTTPS the way main.go does (ServeTLS with TLSConfig).
func TestTLS(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	first := writeCert(t, dir)
	hash, err := config.HashPassword("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	src := "[yard]\ncert = \"yard.crt\"\nkey = \"yard.key\"\n[admin]\npassword_hash = \"" + hash + "\"\n" +
		strings.ReplaceAll(echoServer, "EXE", strings.ReplaceAll(exe, `\`, `\\`))
	path := filepath.Join(dir, config.FileName)
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(path, cfg, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: s.Handler(), TLSConfig: s.TLSConfig(), ErrorLog: log.New(io.Discard, "", 0)}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go hs.ServeTLS(ln, "", "")
	t.Cleanup(func() {
		hs.Close()
		s.Close()
	})
	base := "https://" + ln.Addr().String()

	// A new client per request, so each one makes a new TLS handshake.
	send := func(method, u, body string) (*http.Response, string) {
		t.Helper()
		c := &http.Client{
			Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		defer c.CloseIdleConnections()
		req, _ := http.NewRequest(method, u, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, string(b)
	}

	res, body := send(http.MethodGet, base+"/echo/a", "")
	if res.StatusCode != 200 || !strings.Contains(body, "proto=https") {
		t.Errorf("forwarded over https = %d %q, want X-Forwarded-Proto https", res.StatusCode, body)
	}
	if !bytes.Equal(res.TLS.PeerCertificates[0].Raw, first) {
		t.Error("served certificate is not yard.crt")
	}

	res, _ = send(http.MethodPost, base+"/.yard/login", url.Values{"password": {"s3cret"}}.Encode())
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("login = %d", res.StatusCode)
	}
	if c := res.Cookies(); len(c) == 0 || !c[0].Secure {
		t.Errorf("session cookie over https is not Secure: %v", c)
	}

	// A renewed certificate is used after a reload, without a restart.
	renewed := writeCert(t, dir)
	notices, err := s.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 0 {
		t.Errorf("notices after renewing the certificate: %v", notices)
	}
	res, _ = send(http.MethodGet, base+"/echo/a", "")
	if !bytes.Equal(res.TLS.PeerCertificates[0].Raw, renewed) {
		t.Error("renewed certificate is not served after reload")
	}

	// Turning HTTPS off needs a restart, and the running server keeps its certificate.
	noTLS := strings.Replace(src, "cert = \"yard.crt\"\nkey = \"yard.key\"\n", "", 1)
	notices, err = s.SaveConfig([]byte(noTLS))
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "HTTPS") {
		t.Errorf("notices after turning HTTPS off: %v", notices)
	}
	if res, _ = send(http.MethodGet, base+"/echo/a", ""); !bytes.Equal(res.TLS.PeerCertificates[0].Raw, first) {
		t.Error("certificate from startup is not kept")
	}
}

func TestRedirectHandler(t *testing.T) {
	cases := []struct {
		addr, method, host, target, want string
		code                             int
	}{
		{":443", "GET", "yard.local", "/memo/?q=1", "https://yard.local/memo/?q=1", http.StatusMovedPermanently},
		{":443", "GET", "yard.local:80", "/", "https://yard.local/", http.StatusMovedPermanently},
		{":8443", "GET", "192.168.1.2:8080", "/a", "https://192.168.1.2:8443/a", http.StatusMovedPermanently},
		{":443", "POST", "yard.local", "/a", "https://yard.local/a", http.StatusPermanentRedirect},
		{":443", "GET", "[::1]:80", "/", "https://[::1]/", http.StatusMovedPermanently},
		{":8443", "GET", "[::1]", "/", "https://[::1]:8443/", http.StatusMovedPermanently},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "http://placeholder"+c.target, nil)
		r.Host = c.host
		w := httptest.NewRecorder()
		RedirectHandler(c.addr).ServeHTTP(w, r)
		if w.Code != c.code || w.Header().Get("Location") != c.want {
			t.Errorf("%s %s%s (addr %s) = %d %q, want %d %q",
				c.method, c.host, c.target, c.addr, w.Code, w.Header().Get("Location"), c.code, c.want)
		}
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.Host = ""
	w := httptest.NewRecorder()
	RedirectHandler(":443").ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("no Host = %d", w.Code)
	}
}
