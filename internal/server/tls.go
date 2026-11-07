package server

import (
	"crypto/tls"
	"net"
	"net/http"
	"strings"
)

// TLSConfig returns the TLS settings for addr when yard was started with HTTPS, otherwise nil.
// The certificate is taken from the current configuration on each handshake, so a renewed
// certificate takes effect when the configuration is reloaded or saved, without a restart.
// If the configuration no longer has one, the certificate from startup keeps being used.
func (s *Server) TLSConfig() *tls.Config {
	if s.startCert == nil {
		return nil
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			if c := s.Config().Yard.Certificate(); c != nil {
				return c, nil
			}
			return s.startCert, nil
		},
	}
}

// RedirectHandler redirects every request to the same URL on https, at the port of httpsAddr.
// It is served on yard.redirect_addr.
func RedirectHandler(httpsAddr string) http.Handler {
	_, port, _ := net.SplitHostPort(httpsAddr)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		if host == "" {
			http.Error(w, "Bad Request: missing Host", http.StatusBadRequest)
			return
		}
		if port != "443" {
			host = net.JoinHostPort(host, port)
		} else if strings.Contains(host, ":") {
			host = "[" + host + "]" // IPv6 literal
		}
		code := http.StatusMovedPermanently
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			code = http.StatusPermanentRedirect
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), code)
	})
}
