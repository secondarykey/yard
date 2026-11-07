package server

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"yard/internal/config"
	"yard/internal/proc"
)

//go:embed templates/*.html
var templateFS embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"uptime": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return formatDuration(time.Since(t))
	},
	"rfc3339": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Format(time.RFC3339)
	},
	"admin": func() string { return config.AdminPath },
}).ParseFS(templateFS, "templates/*.html"))

// logTailBytes はログ表示で読む末尾のバイト数。
const logTailBytes = 256 << 10

// 管理画面の URL 構成（すべて /.yard 以下）。
//
//	GET  /health                    yard の生死と各ツールの状態（接続元制限なし）
//	GET  /health/{name}             ツールが running なら 200、そうでなければ 503（接続元制限なし）
//	GET  /                          ツール一覧
//	POST /tools/{name}/{action}     start / stop / restart
//	GET  /tools/{name}/log          ツールの出力（末尾）
//	GET  /config                    設定ファイルの編集
//	POST /config                    検証して保存し、反映する
//	POST /reload                    設定ファイルを読み直して反映する
//	GET  /api/tools                 JSON
//	POST /api/tools/{name}/{action} JSON
//	POST /api/reload                JSON
//	GET  /api/config                設定ファイルの本文
//	PUT  /api/config                検証して保存し、反映する
func (s *Server) adminHandler() http.Handler {
	a := config.AdminPath

	priv := http.NewServeMux()
	priv.HandleFunc("GET "+a+"/{$}", s.handleDashboard)
	priv.HandleFunc("POST "+a+"/tools/{name}/{action}", s.handleAction)
	priv.HandleFunc("GET "+a+"/tools/{name}/log", s.handleLog)
	priv.HandleFunc("GET "+a+"/config", s.handleConfigPage)
	priv.HandleFunc("POST "+a+"/config", s.handleConfigSave)
	priv.HandleFunc("POST "+a+"/reload", s.handleReload)
	priv.HandleFunc("GET "+a+"/api/tools", s.apiTools)
	priv.HandleFunc("POST "+a+"/api/tools/{name}/{action}", s.apiAction)
	priv.HandleFunc("POST "+a+"/api/reload", s.apiReload)
	priv.HandleFunc("GET "+a+"/api/config", s.apiConfigGet)
	priv.HandleFunc("PUT "+a+"/api/config", s.apiConfigPut)
	priv.HandleFunc(a+"/", func(w http.ResponseWriter, r *http.Request) {
		s.renderError(w, r, http.StatusNotFound, "Not Found", "")
	})

	// 設定の書き換えはプログラムの実行と同義なので、別サイトのページから
	// フォームを送らせる攻撃（CSRF）を Sec-Fetch-Site / Origin で弾く。
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.renderError(w, r, http.StatusForbidden, "Forbidden", "cross-origin request")
	}))

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+a+"/health", s.handleHealth)
	mux.HandleFunc("GET "+a+"/health/{name}", s.handleHealthTool)
	mux.Handle(a+"/", s.restrict(cop.Handler(priv)))
	return mux
}

// restrict は admin.allow に含まれない接続元を拒否する。
func (s *Server) restrict(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.Config()
		if !cfg.Admin.Allowed(r.RemoteAddr) {
			s.renderError(w, r, http.StatusForbidden, "Forbidden", "")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// ---- 生死確認 ----

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	type tool struct {
		Name  string     `json:"name"`
		Path  string     `json:"path"`
		State proc.State `json:"state"`
	}
	var tools []tool
	for _, st := range s.mgr.Statuses() {
		tools = append(tools, tool{st.Name, st.Path, st.State})
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "tools": tools})
}

func (s *Server) handleHealthTool(w http.ResponseWriter, r *http.Request) {
	p, ok := s.mgr.Get(r.PathValue("name"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown tool"})
		return
	}
	st := p.Status()
	code := http.StatusOK
	if st.State != proc.StateRunning {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"name": st.Name, "state": st.State})
}

// ---- 画面 ----

type dashboardData struct {
	Page    string
	Tools   []proc.Status
	Addr    string
	Config  string
	Notices []string
	Error   string
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "dashboard.html", dashboardData{
		Page:    "tools",
		Tools:   s.mgr.Statuses(),
		Addr:    s.addr,
		Config:  s.path,
		Notices: r.URL.Query()["notice"],
		Error:   r.URL.Query().Get("error"),
	})
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	q := url.Values{}
	if _, err := s.doAction(r.PathValue("name"), r.PathValue("action")); err != nil {
		q.Set("error", err.Error())
	}
	redirectDashboard(w, r, q)
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	q := url.Values{}
	notices, err := s.Reload()
	if err != nil {
		q.Set("error", err.Error())
	}
	q["notice"] = notices
	redirectDashboard(w, r, q)
}

func redirectDashboard(w http.ResponseWriter, r *http.Request, q url.Values) {
	u := config.AdminPath + "/"
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
}

type logData struct {
	Page      string
	Name      string
	File      string
	Text      string
	Truncated bool
	Error     string
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	p, ok := s.mgr.Get(r.PathValue("name"))
	if !ok {
		s.renderError(w, r, http.StatusNotFound, "Not Found", "")
		return
	}
	t := p.Tool()
	d := logData{Page: "tools", Name: t.Name, File: t.Log}
	if t.Log == "" {
		d.Error = "log is not configured"
	} else if text, truncated, err := tail(t.Log, logTailBytes); err != nil {
		d.Error = err.Error()
	} else {
		d.Text, d.Truncated = text, truncated
	}
	if r.URL.Query().Has("raw") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, d.Text)
		return
	}
	s.render(w, http.StatusOK, "log.html", d)
}

type configData struct {
	Page    string
	File    string
	Text    string
	Saved   bool
	Notices []string
	Error   string
}

func (s *Server) handleConfigPage(w http.ResponseWriter, r *http.Request) {
	d := configData{Page: "config", File: s.path}
	b, err := s.ReadConfig()
	if err != nil {
		d.Error = err.Error()
	}
	d.Text = string(b)
	s.render(w, http.StatusOK, "config.html", d)
}

func (s *Server) handleConfigSave(w http.ResponseWriter, r *http.Request) {
	text := r.PostFormValue("config")
	d := configData{Page: "config", File: s.path, Text: text}
	notices, err := s.SaveConfig([]byte(text))
	if err != nil {
		d.Error = err.Error()
		s.render(w, http.StatusBadRequest, "config.html", d)
		return
	}
	d.Saved, d.Notices = true, notices
	s.render(w, http.StatusOK, "config.html", d)
}

// ---- JSON API ----

func (s *Server) apiTools(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.Statuses())
}

func (s *Server) apiAction(w http.ResponseWriter, r *http.Request) {
	st, err := s.doAction(r.PathValue("name"), r.PathValue("action"))
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errUnknownTool) {
			code = http.StatusNotFound
		}
		writeJSON(w, code, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) apiReload(w http.ResponseWriter, r *http.Request) {
	notices, err := s.Reload()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notices": notices, "tools": s.mgr.Statuses()})
}

func (s *Server) apiConfigGet(w http.ResponseWriter, r *http.Request) {
	b, err := s.ReadConfig()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/toml; charset=utf-8")
	w.Write(b)
}

func (s *Server) apiConfigPut(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	notices, err := s.SaveConfig(b)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notices": notices})
}

// ---- 共通 ----

var errUnknownTool = errors.New("unknown tool")

func (s *Server) doAction(name, action string) (proc.Status, error) {
	p, ok := s.mgr.Get(name)
	if !ok {
		return proc.Status{}, fmt.Errorf("%w: %s", errUnknownTool, name)
	}
	var err error
	switch action {
	case "start":
		err = p.Start()
	case "stop":
		p.Stop()
	case "restart":
		err = p.Restart()
	default:
		return proc.Status{}, fmt.Errorf("unknown action: %s", action)
	}
	if err != nil {
		return proc.Status{}, fmt.Errorf("%s: %w", name, err)
	}
	s.logger.Printf("[%s] %s requested", name, action)
	return p.Status(), nil
}

func (s *Server) render(w http.ResponseWriter, code int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.logger.Printf("template %s: %v", name, err)
	}
}

type errorData struct {
	Code   int
	Title  string
	Detail string
}

// renderError はブラウザには HTML、それ以外にはテキストでエラーを返す。
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, code int, title, detail string) {
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(code)
		msg := fmt.Sprintf("%d %s", code, title)
		if detail != "" {
			msg += ": " + detail
		}
		io.WriteString(w, msg+"\n")
		return
	}
	s.render(w, code, "error.html", errorData{code, title, detail})
}

type portalData struct {
	Tools []proc.Status
}

func (s *Server) renderPortal(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "portal.html", portalData{Tools: s.portalTools()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

// tail はファイルの末尾 n バイトを返す。途中で切った場合は最初の改行までを捨てる。
func tail(name string, n int64) (string, bool, error) {
	f, err := os.Open(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	off := max(fi.Size()-n, 0)
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return "", false, err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", false, err
	}
	s := string(b)
	if off > 0 {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	return strings.ToValidUTF8(s, "�"), off > 0, nil
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	day := d / (24 * time.Hour)
	d -= day * 24 * time.Hour
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	sec := (d - m*time.Minute) / time.Second
	switch {
	case day > 0:
		return fmt.Sprintf("%dd %dh", day, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, sec)
	default:
		return fmt.Sprintf("%ds", sec)
	}
}
