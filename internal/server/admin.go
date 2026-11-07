package server

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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

// logTailBytes is how many bytes from the end of a log are shown.
const logTailBytes = 256 << 10

// Admin UI URLs (all under /.yard).
//
//	GET  /health                      whether yard is alive and each server's state (no client restriction)
//	GET  /health/{name}               200 if the server is running, otherwise 503 (no client restriction)
//	GET  /                            server list
//	POST /servers/{name}/{action}     start / stop / restart
//	GET  /servers/{name}/log          server output (tail)
//	GET  /config                      edit the configuration file
//	POST /config                      validate, save, and apply
//	POST /reload                      reread and apply the configuration file
//	GET  /matrix                      digital rain (just for fun)
//	POST /login                       log in with the form value "password" (when admin.password_hash is set)
//	POST /logout                      log out
//	GET  /api/servers                 JSON
//	POST /api/servers/{name}/{action} JSON
//	POST /api/reload                  JSON
//	GET  /api/config                  contents of the configuration file
//	PUT  /api/config                  validate, save, and apply (TOML, or JSON from the form editor)
//
// With a password set, everything except /health and /login needs a session; browsers that
// have none get the digital rain as the login page.
func (s *Server) adminHandler() http.Handler {
	a := config.AdminPath

	priv := http.NewServeMux()
	priv.HandleFunc("GET "+a+"/{$}", s.handleDashboard)
	priv.HandleFunc("POST "+a+"/servers/{name}/{action}", s.handleAction)
	priv.HandleFunc("GET "+a+"/servers/{name}/log", s.handleLog)
	priv.HandleFunc("GET "+a+"/config", s.handleConfigPage)
	priv.HandleFunc("POST "+a+"/config", s.handleConfigSave)
	priv.HandleFunc("POST "+a+"/reload", s.handleReload)
	priv.HandleFunc("GET "+a+"/matrix", s.handleMatrix)
	priv.HandleFunc("POST "+a+"/logout", s.handleLogout)
	priv.HandleFunc("GET "+a+"/api/servers", s.apiServers)
	priv.HandleFunc("POST "+a+"/api/servers/{name}/{action}", s.apiAction)
	priv.HandleFunc("POST "+a+"/api/reload", s.apiReload)
	priv.HandleFunc("GET "+a+"/api/config", s.apiConfigGet)
	priv.HandleFunc("PUT "+a+"/api/config", s.apiConfigPut)
	priv.HandleFunc(a+"/", func(w http.ResponseWriter, r *http.Request) {
		s.renderError(w, r, http.StatusNotFound, "Not Found", "")
	})

	// Logging in is the only thing allowed without a session.
	login := http.NewServeMux()
	login.HandleFunc("POST "+a+"/login", s.handleLogin)
	login.Handle(a+"/", s.authenticate(priv))

	// Editing the configuration is equivalent to running programs, so reject forms
	// submitted from other sites' pages (CSRF) using Sec-Fetch-Site / Origin.
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.renderError(w, r, http.StatusForbidden, "Forbidden", "cross-origin request")
	}))

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+a+"/health", s.handleHealth)
	mux.HandleFunc("GET "+a+"/health/{name}", s.handleHealthServer)
	mux.Handle(a+"/", s.restrict(cop.Handler(login)))
	return mux
}

// restrict rejects clients not listed in admin.allow.
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

// ---- Health checks ----

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	type server struct {
		Name  string     `json:"name"`
		Path  string     `json:"path"`
		State proc.State `json:"state"`
	}
	var servers []server
	for _, st := range s.mgr.Statuses() {
		servers = append(servers, server{st.Name, st.Path, st.State})
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "servers": servers})
}

func (s *Server) handleHealthServer(w http.ResponseWriter, r *http.Request) {
	p, ok := s.mgr.Get(r.PathValue("name"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown server"})
		return
	}
	st := p.Status()
	code := http.StatusOK
	if st.State != proc.StateRunning {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"name": st.Name, "state": st.State})
}

// ---- Pages ----

// pageData is what the header needs. render fills in Auth.
type pageData struct {
	Page string
	Auth bool // login is on, so show "Log out"
}

func (p *pageData) setAuth(on bool) { p.Auth = on }

type dashboardData struct {
	pageData
	Servers []proc.Status
	Addr    string
	Config  string
	Notices []string
	Error   string
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "dashboard.html", &dashboardData{
		pageData: pageData{Page: "servers"},
		Servers:  s.mgr.Statuses(),
		Addr:     s.addr,
		Config:   s.path,
		Notices:  r.URL.Query()["notice"],
		Error:    r.URL.Query().Get("error"),
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
	pageData
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
	file := p.LogFile()
	d := &logData{pageData: pageData{Page: "servers"}, Name: p.Config().Name, File: file}
	if file == "" {
		d.Error = "no log has been written"
	} else if text, truncated, err := tail(file, logTailBytes); err != nil {
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
	pageData
	View     string // "form" or "text"
	File     string
	Dir      string         // directory of the configuration file (base for relative command / dir)
	Form     *config.Config // the configuration as written, for the form view
	Password bool           // the file sets admin.password_hash (which is not part of Form's JSON)
	Text     string
	Saved    bool
	Notices  []string
	Error    string
}

func (s *Server) handleConfigPage(w http.ResponseWriter, r *http.Request) {
	d := &configData{pageData: pageData{Page: "config"}, View: "form", File: s.path, Dir: filepath.Dir(s.path)}
	if r.URL.Query().Get("view") == "text" {
		d.View = "text"
	}
	b, err := s.ReadConfig()
	if err != nil {
		d.Error = err.Error()
	}
	d.Text = string(b)
	if d.View == "form" {
		if d.Form, err = config.Decode(b); err != nil {
			// The form cannot represent a file that does not decode; fix it as text.
			d.View, d.Form, d.Error = "text", nil, err.Error()
		} else {
			d.Password = d.Form.Admin.PasswordHash != ""
		}
	}
	s.render(w, http.StatusOK, "config.html", d)
}

func (s *Server) handleConfigSave(w http.ResponseWriter, r *http.Request) {
	text := r.PostFormValue("config")
	d := &configData{pageData: pageData{Page: "config"}, View: "text", File: s.path, Dir: filepath.Dir(s.path), Text: text}
	notices, err := s.SaveConfig([]byte(text))
	if err != nil {
		d.Error = err.Error()
		s.render(w, http.StatusBadRequest, "config.html", d)
		return
	}
	d.Saved, d.Notices = true, notices
	s.render(w, http.StatusOK, "config.html", d)
}

type matrixData struct {
	Login bool   // show the password box on click
	Next  string // where to go after logging in
}

func (s *Server) handleMatrix(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "matrix.html", matrixData{})
}

// ---- JSON API ----

func (s *Server) apiServers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.Statuses())
}

func (s *Server) apiAction(w http.ResponseWriter, r *http.Request) {
	st, err := s.doAction(r.PathValue("name"), r.PathValue("action"))
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errUnknownServer) {
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
	writeJSON(w, http.StatusOK, map[string]any{"notices": notices, "servers": s.mgr.Statuses()})
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

// configForm is the JSON the form editor sends: the keys of yard.ini, except that admin.password_hash
// never leaves the server. "password" is absent (or null) to keep the current one, "" to turn login
// off, and anything else to set a new password.
type configForm struct {
	config.Config
	Password *string `json:"password"`
}

// apiConfigPut takes the configuration text, or with Content-Type application/json the configuration
// as the form editor sends it (configForm), which is written out with config.Format.
// Setting a password from the form also logs in the client that set it.
func (s *Server) apiConfigPut(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	newHash := ""
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "application/json" {
		var f configForm
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&f); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		switch {
		case f.Password == nil:
			hash, err := s.currentPasswordHash()
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "cannot keep the current password: " + err.Error()})
				return
			}
			f.Admin.PasswordHash = hash
		case *f.Password != "":
			if f.Admin.PasswordHash, err = config.HashPassword(*f.Password); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			newHash = f.Admin.PasswordHash
		}
		b = config.Format(&f.Config)
	}
	notices, err := s.SaveConfig(b)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if newHash != "" {
		s.startSession(w, r, newHash)
	}
	writeJSON(w, http.StatusOK, map[string]any{"notices": notices})
}

// currentPasswordHash returns admin.password_hash as written in the configuration file.
func (s *Server) currentPasswordHash() (string, error) {
	b, err := s.ReadConfig()
	if err != nil {
		return "", err
	}
	c, err := config.Decode(b)
	if err != nil {
		return "", err
	}
	return c.Admin.PasswordHash, nil
}

// ---- Common ----

var errUnknownServer = errors.New("unknown server")

func (s *Server) doAction(name, action string) (proc.Status, error) {
	p, ok := s.mgr.Get(name)
	if !ok {
		return proc.Status{}, fmt.Errorf("%w: %s", errUnknownServer, name)
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
	s.logger.Info(action+" requested", "server", name)
	return p.Status(), nil
}

func (s *Server) render(w http.ResponseWriter, code int, name string, data any) {
	if p, ok := data.(interface{ setAuth(bool) }); ok {
		p.setAuth(s.Config().Admin.AuthEnabled())
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.logger.Error("template", "name", name, "err", err)
	}
}

type errorData struct {
	Code   int
	Title  string
	Detail string
}

// renderError returns the error as HTML to browsers and as plain text otherwise.
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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

// tail returns the last n bytes of a file. If truncated, it drops everything up to the first newline.
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
