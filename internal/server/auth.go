package server

import (
	"crypto/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"yard/internal/config"
)

// Login for /.yard. It is on when admin.password_hash is set, and applies on top of admin.allow.
//
// Sessions live in memory only, so restarting yard logs everyone out. A session remembers the
// password hash it logged in with, so changing or removing the password ends every session.
const (
	sessionCookie = "yard_session"
	sessionIdle   = 24 * time.Hour  // a session ends after this long without requests
	loginDelay    = 1 * time.Second // added to every failed login
)

type session struct {
	hash string // admin.password_hash at login
	seen time.Time
}

type sessions struct {
	mu sync.Mutex
	m  map[string]*session

	// loginMu lets one password check run at a time, so that together with loginDelay
	// guessing is limited to about one attempt per second.
	loginMu sync.Mutex
}

// valid reports whether token is a live session for hash, and extends it.
func (ss *sessions) valid(token, hash string) bool {
	if token == "" {
		return false
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	se, ok := ss.m[token]
	if !ok {
		return false
	}
	now := time.Now()
	if se.hash != hash || now.Sub(se.seen) > sessionIdle {
		delete(ss.m, token)
		return false
	}
	se.seen = now
	return true
}

func (ss *sessions) create(hash string) string {
	token := rand.Text()
	now := time.Now()
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.m == nil {
		ss.m = map[string]*session{}
	}
	for t, se := range ss.m {
		if se.hash != hash || now.Sub(se.seen) > sessionIdle {
			delete(ss.m, t)
		}
	}
	ss.m[token] = &session{hash: hash, seen: now}
	return token
}

func (ss *sessions) remove(token string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	delete(ss.m, token)
}

// loggedIn reports whether the request may use /.yard: login is off, or it carries a valid session.
func (s *Server) loggedIn(r *http.Request) bool {
	cfg := s.Config()
	if !cfg.Admin.AuthEnabled() {
		return true
	}
	c, err := r.Cookie(sessionCookie)
	return err == nil && s.sessions.valid(c.Value, cfg.Admin.PasswordHash)
}

// startSession logs the client in for the given password hash.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, hash string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    s.sessions.create(hash),
		Path:     config.AdminPath,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
}

// authenticate lets logged-in requests through. Others get the login page (the digital rain)
// if they come from a browser, and 401 otherwise.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.loggedIn(r) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, config.AdminPath+"/api/") {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "login required"})
			return
		}
		if !strings.Contains(r.Header.Get("Accept"), "text/html") {
			s.renderError(w, r, http.StatusUnauthorized, "Unauthorized", "login required")
			return
		}
		// After logging in, come back here. A form that was posted cannot be repeated, so go to the top instead.
		next := config.AdminPath + "/"
		if r.Method == http.MethodGet && r.URL.Path != config.AdminPath+"/matrix" {
			next = r.URL.RequestURI()
		}
		s.render(w, http.StatusUnauthorized, "matrix.html", matrixData{Login: true, Next: next})
	})
}

// handleLogin checks the form value "password" and starts a session.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	password := r.PostFormValue("password")

	s.sessions.loginMu.Lock()
	defer s.sessions.loginMu.Unlock()
	cfg := s.Config()
	if !cfg.Admin.AuthEnabled() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !cfg.Admin.CheckPassword(password) {
		time.Sleep(loginDelay)
		s.logger.Warn("login failed", "remote", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "wrong password"})
		return
	}
	s.startSession(w, r, cfg.Admin.PasswordHash)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.remove(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: config.AdminPath, MaxAge: -1})
	http.Redirect(w, r, config.AdminPath+"/", http.StatusSeeOther)
}
