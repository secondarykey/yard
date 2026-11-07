package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// proxy forwards the request to the server whose path matches.
//
// The Host header is passed through unchanged, and X-Forwarded-For / -Host / -Proto and
// X-Forwarded-Prefix (the server's path) are added. Upgrades such as WebSocket are
// relayed as is by httputil.ReverseProxy.
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
			pr.SetURL(&url.URL{Scheme: "http", Host: target})
			if t.StripPrefix && t.Path != "/" {
				stripPrefix(pr.Out.URL, pr.In.URL, t.Path)
			}
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Forwarded-Prefix", t.Path)
		},
		Transport: s.transport,
		ErrorLog:  s.logger,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return // the client disconnected
			}
			s.logger.Printf("[%s] proxy: %v", t.Name, err)
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
