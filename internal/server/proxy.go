package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// proxy は path に一致するツールへリクエストを転送する。
//
// Host ヘッダは元のまま渡し、X-Forwarded-For / -Host / -Proto と
// X-Forwarded-Prefix（ツールの path）を付ける。WebSocket などの Upgrade も
// httputil.ReverseProxy がそのまま中継する。
func (s *Server) proxy(w http.ResponseWriter, r *http.Request) {
	p, ok := s.mgr.Match(r.URL.Path)
	if !ok {
		// "/" も含め、どのツールにも一致しなければ 404。
		// yard 自身の画面は /.yard 以外には出さない。
		s.renderError(w, r, http.StatusNotFound, "Not Found", "")
		return
	}
	t := p.Tool()
	setTool(r, t.Name)

	// 取り除いた結果が空のパスになると、ツール側の相対リンクが壊れるので
	// "/memo" は "/memo/" に揃える。取り除かない場合はツールの判断に任せる。
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
				return // 利用者が接続を切った
			}
			s.logger.Printf("[%s] proxy: %v", t.Name, err)
			s.renderError(w, r, http.StatusBadGateway, "Bad Gateway", t.Name)
		},
	}
	if t.StripPrefix && t.Path != "/" {
		rp.ModifyResponse = func(res *http.Response) error {
			// ツールが自分の "/" 基準で返したリダイレクト先に公開パスを付け直す。
			if loc := res.Header.Get("Location"); strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") {
				res.Header.Set("Location", t.Path+loc)
			}
			return nil
		}
	}
	rp.ServeHTTP(w, r)
}

// stripPrefix は out のパスから prefix を取り除く。in は元のリクエスト URL。
func stripPrefix(out, in *url.URL, prefix string) {
	p := strings.TrimPrefix(in.Path, prefix)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	out.Path = p
	out.RawPath = ""
	if in.RawPath != "" {
		// %2F などのエンコードを保つ。prefix は '%' を含まない（設定で検証済み）。
		raw := strings.TrimPrefix(in.RawPath, prefix)
		if !strings.HasPrefix(raw, "/") {
			raw = "/" + raw
		}
		out.RawPath = raw
	}
}
