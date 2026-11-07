package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCert writes a self-signed certificate and its key as dir/yard.crt and dir/yard.key.
func writeCert(t *testing.T, dir string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "yard test"},
		DNSNames:     []string{"localhost"},
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
}

func TestParseTLS(t *testing.T) {
	dir := t.TempDir()
	writeCert(t, dir)
	c, err := Parse([]byte(`
[yard]
addr = ":443"
cert = "yard.crt"
key = "yard.key"
redirect_addr = ":80"`), dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !c.Yard.TLS() || c.Yard.Certificate() == nil {
		t.Fatal("certificate not loaded")
	}
	if want := filepath.Join(dir, "yard.crt"); c.Yard.Cert != want {
		t.Errorf("cert = %q, want %q (relative to the yard.ini directory)", c.Yard.Cert, want)
	}

	c, err = Parse([]byte(`[yard]`), dir, dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Yard.TLS() || c.Yard.Certificate() != nil {
		t.Error("HTTPS without cert / key")
	}
}

func TestParseTLSErrors(t *testing.T) {
	dir := t.TempDir()
	writeCert(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "broken.crt"), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"must be set together": "[yard]\ncert = \"yard.crt\"",
		"needs yard.cert":      "[yard]\nredirect_addr = \":80\"",
		"yard.cert / yard.key": "[yard]\ncert = \"broken.crt\"\nkey = \"yard.key\"",
		"yard.redirect_addr":   "[yard]\ncert = \"yard.crt\"\nkey = \"yard.key\"\nredirect_addr = \"80\"",
		"missing.crt":          "[yard]\ncert = \"missing.crt\"\nkey = \"yard.key\"",
	}
	for want, src := range cases {
		_, err := Parse([]byte(src), dir, dir)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
}
