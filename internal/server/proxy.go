package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"yard/internal/config"
)

// proxy forwards the request to the server whose path matches.
//
// The Host header is passed through unchanged, and X-Forwarded-For / -Host / -Proto,
// X-Forwarded-Prefix (the server's path), and Forwarded (RFC 7239) are added. Any of these
// sent by the client are dropped first, so they carry only what Yard saw. Upgrades such as WebSocket are
// relayed as is by httputil.ReverseProxy. A server with https is reached over TLS.
//
// A server with local_only gets only requests from this machine; others get 403.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request) {
	p, ok := s.mgr.Match(r.URL.Path)
	if !ok {
		// 404 if no server matches, including "/".
		// yard's own pages are only served under /.yard.
		s.renderError(w, r, http.StatusNotFound, "Not Found", "")
		return
	}
	t := p.Config()
	setTarget(r, t.Name)
	if t.LocalOnly && !config.IsLoopback(r.RemoteAddr) {
		s.renderError(w, r, http.StatusForbidden, "Forbidden", t.Name+" is available from this machine only")
		return
	}

	// Stripping down to an empty path would break the server's relative links,
	// so redirect "/memo" to "/memo/". Without stripping, leave it to the server.
	if t.StripPrefix && t.Path != "/" && r.URL.Path == t.Path {
		u := *r.URL
		u.Path, u.RawPath = t.Path+"/", ""
		code := http.StatusMovedPermanently
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			code = http.StatusPermanentRedirect
		}
		http.Redirect(w, r, u.String(), code)
		return
	}

	target := waitReady(r.Context(), p)
	if target == "" {
		if r.Context().Err() != nil {
			return
		}
		st := p.Status()
		w.Header().Set("Retry-After", "5")
		s.renderError(w, r, http.StatusServiceUnavailable, "Service Unavailable", t.Name+": "+string(st.State))
		return
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			scheme := "http"
			if t.HTTPS {
				scheme = "https"
			}
			pr.SetURL(&url.URL{Scheme: scheme, Host: target})
			if t.StripPrefix && t.Path != "/" {
				stripPrefix(pr.Out.URL, pr.In.URL, t.Path)
			}
			pr.Out.Host = pr.In.Host
			// Rewrite has already removed the client's Forwarded / X-Forwarded-* headers.
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Forwarded-Prefix", t.Path)
			pr.Out.Header.Set("Forwarded", forwarded(pr.In))
		},
		Transport: s.transport,
		ErrorLog:  slog.NewLogLogger(s.logger.Handler(), slog.LevelWarn),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return // the client disconnected
			}
			s.logger.Warn("proxy", "server", t.Name, "err", err)
			s.renderError(w, r, http.StatusBadGateway, "Bad Gateway", t.Name)
		},
	}
	if t.StripPrefix && t.Path != "/" {
		rp.ModifyResponse = func(res *http.Response) error {
			// Prepend the public path to redirects the server returned relative to its own "/".
			if loc := res.Header.Get("Location"); strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") {
				res.Header.Set("Location", t.Path+loc)
			}
			return nil
		}
	}
	rp.ServeHTTP(w, r)
}

// stripPrefix removes prefix from out's path. in is the original request URL.
func stripPrefix(out, in *url.URL, prefix string) {
	p := strings.TrimPrefix(in.Path, prefix)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	out.Path = p
	out.RawPath = ""
	if in.RawPath != "" {
		// Keep encodings such as %2F. prefix never contains '%' (validated in config).
		raw := strings.TrimPrefix(in.RawPath, prefix)
		if !strings.HasPrefix(raw, "/") {
			raw = "/" + raw
		}
		out.RawPath = raw
	}
}

// forwarded returns the Forwarded header (RFC 7239) for r: the same client address,
// host, and scheme as X-Forwarded-For / -Host / -Proto.
func forwarded(r *http.Request) string {
	var parts []string
	if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if strings.Contains(ip, ":") {
			ip = "[" + ip + "]" // IPv6 is written in brackets (and therefore quoted)
		}
		parts = append(parts, "for="+forwardedValue(ip))
	}
	if r.Host != "" {
		parts = append(parts, "host="+forwardedValue(r.Host))
	}
	proto := "http"
	if r.TLS != nil {
		proto = "https"
	}
	parts = append(parts, "proto="+proto)
	return strings.Join(parts, ";")
}

// forwardedValue writes v as a token if it is one, otherwise as a quoted-string.
func forwardedValue(v string) string {
	token := v != ""
	for i := 0; i < len(v) && token; i++ {
		token = isTokenChar(v[i])
	}
	if token {
		return v
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(v); i++ {
		if v[i] == '"' || v[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(v[i])
	}
	b.WriteByte('"')
	return b.String()
}

// isTokenChar reports whether c is a tchar of RFC 9110.
func isTokenChar(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	return strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}
