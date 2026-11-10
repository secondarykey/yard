package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	"yard/internal/config"
)

// proxy forwards the request to the server whose path matches.
//
// The Host header is passed through unchanged, and X-Forwarded-For / -Host / -Proto and
// X-Forwarded-Prefix (the server's path) are added. Upgrades such as WebSocket are
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

	target := waitReady(r.Context(), p, t.StartTimeout.Duration)
	if target == "" {
		if r.Context().Err() != nil {
			return
		}
		st := p.Status()
		w.Header().Set("Retry-After", "5")
		s.renderError(w, r, http.StatusServiceUnavailable, "Service Unavailable", t.Name+": "+string(st.State))
		return
	}

	rt := &route{
		name:   t.Name,
		path:   t.Path,
		strip:  t.StripPrefix && t.Path != "/",
		scheme: "http",
		host:   target,
	}
	if t.HTTPS {
		rt.scheme = "https"
	}
	s.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), routeKey{}, rt)))
}

// route is where one request goes. proxy puts it in the request's context for the shared
// ReverseProxy, so that the proxy (and its logger) is not built again for every request.
type route struct {
	name   string // server name
	path   string // public path of the server
	strip  bool   // strip_prefix, and the path is not "/"
	scheme string
	host   string // 127.0.0.1:{port}
}

type routeKey struct{}

func routeOf(r *http.Request) *route { return r.Context().Value(routeKey{}).(*route) }

// newReverseProxy builds the ReverseProxy shared by all servers.
func (s *Server) newReverseProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			rt := routeOf(pr.In)
			pr.SetURL(&url.URL{Scheme: rt.scheme, Host: rt.host})
			if rt.strip {
				stripPrefix(pr.Out.URL, pr.In.URL, rt.path)
			}
			pr.Out.Host = pr.In.Host
			// Rewrite has already removed Forwarded and X-Forwarded-* sent by the client. These two
			// are not removed, and servers that trust them would take a client IP from the client.
			pr.Out.Header.Del("X-Real-Ip")
			pr.Out.Header.Del("True-Client-Ip")
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Forwarded-Prefix", rt.path)
		},
		ModifyResponse: func(res *http.Response) error {
			rt := routeOf(res.Request)
			if !rt.strip {
				return nil
			}
			// Prepend the public path to redirects the server returned relative to its own "/".
			if loc := res.Header.Get("Location"); strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") {
				res.Header.Set("Location", rt.path+loc)
			}
			return nil
		},
		Transport:  s.transport,
		BufferPool: &bufferPool{},
		ErrorLog:   slog.NewLogLogger(s.logger.Handler(), slog.LevelWarn),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return // the client disconnected
			}
			rt := routeOf(r)
			s.logger.Warn("proxy", "server", rt.name, "err", err)
			s.renderError(w, r, http.StatusBadGateway, "Bad Gateway", rt.name)
		},
	}
}

// bufferPool reuses the buffers ReverseProxy copies response bodies through,
// instead of allocating 32 KB for every request.
type bufferPool struct{ p sync.Pool }

func (b *bufferPool) Get() []byte {
	if buf, ok := b.p.Get().(*[]byte); ok {
		return *buf
	}
	return make([]byte, 32*1024)
}

func (b *bufferPool) Put(buf []byte) { b.p.Put(&buf) }

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
