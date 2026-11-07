// Package server is the HTTP side of yard: forwarding to servers (proxy.go),
// the access log (accesslog.go), and the /.yard admin UI (admin.go).
//
// Forwarding headers (X-Forwarded-For etc.) are never used to identify the client,
// since spoofing them would bypass the /.yard client restriction. Only RemoteAddr is used.
package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"yard/internal/config"
	"yard/internal/proc"
)

// Server ties together the configuration, process management, and HTTP handlers.
type Server struct {
	path   string // absolute path of the configuration file
	logger *slog.Logger
	mgr    *proc.Manager

	transport *http.Transport

	mu     sync.RWMutex
	cfg    *config.Config
	access *accessLog
	addr   string // yard.addr at startup; cannot be changed by a reload
	saveMu sync.Mutex

	// Settings from startup that a reload cannot change (only the certificate itself can be renewed).
	startCert    *tls.Certificate // nil without HTTPS
	redirectAddr string

	sessions sessions // logins to /.yard (auth.go)
}

// New applies the configuration and starts the servers.
func New(cfgPath string, cfg *config.Config, logger *slog.Logger) (*Server, error) {
	access, err := openAccessLog(cfg.Yard.AccessLog)
	if err != nil {
		return nil, fmt.Errorf("access log: %w", err)
	}
	s := &Server{
		path:   cfgPath,
		logger: logger,
		mgr:    proc.NewManager(logger),
		cfg:    cfg,
		access: access,
		addr:   cfg.Yard.Addr,

		startCert:    cfg.Yard.Certificate(),
		redirectAddr: cfg.Yard.RedirectAddr,
		transport: &http.Transport{
			Proxy: nil, // targets are on loopback, so ignore proxy settings from the environment
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			// Only for servers with https. The target is 127.0.0.1:{port}, which no certificate
			// names, and the connection never leaves this machine, so the certificate is not verified.
			// Setting TLSClientConfig also keeps the transport on HTTP/1.1, which upgrades need.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	s.mgr.Apply(cfg.Servers)
	return s, nil
}

// Config returns the current configuration.
func (s *Server) Config() *config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Manager returns the process manager.
func (s *Server) Manager() *proc.Manager { return s.mgr }

// Close stops all servers and closes the logs.
func (s *Server) Close() {
	s.mgr.StopAll()
	s.transport.CloseIdleConnections()
	s.mu.Lock()
	s.access.Close()
	s.access = nil
	s.mu.Unlock()
}

// Reload rereads and applies the configuration file.
// The returned notices describe settings that could not be applied (and need a restart).
func (s *Server) Reload() (notices []string, err error) {
	cfg, err := config.Load(s.path, s.Config().LogDir)
	if err != nil {
		return nil, err
	}
	return s.apply(cfg)
}

// ReadConfig returns the contents of the configuration file.
func (s *Server) ReadConfig() ([]byte, error) {
	return os.ReadFile(s.path)
}

// SaveConfig validates the contents, writes them to the configuration file, and applies them.
// If validation fails, the file is left unchanged.
func (s *Server) SaveConfig(data []byte) (notices []string, err error) {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	cfg, err := config.Parse(data, filepath.Dir(s.path), s.Config().LogDir)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(s.path, data); err != nil {
		return nil, err
	}
	s.logger.Info("config saved", "path", s.path)
	return s.apply(cfg)
}

func (s *Server) apply(cfg *config.Config) ([]string, error) {
	var notices []string
	old := s.Config()
	if cfg.Yard.Addr != s.addr {
		notices = append(notices, fmt.Sprintf("yard.addr (%s → %s) takes effect after restarting yard", s.addr, cfg.Yard.Addr))
	}
	if cfg.Yard.TLS() != (s.startCert != nil) {
		notices = append(notices, "turning HTTPS (yard.cert / yard.key) on or off takes effect after restarting yard")
	}
	if cfg.Yard.RedirectAddr != s.redirectAddr {
		notices = append(notices, "yard.redirect_addr takes effect after restarting yard")
	}
	if cfg.Yard.Log != old.Yard.Log {
		notices = append(notices, "yard.log takes effect after restarting yard")
	}
	if cfg.Service != old.Service {
		notices = append(notices, "[service] takes effect after re-registering the service (uninstall → install)")
	}

	var access *accessLog
	if cfg.Yard.AccessLog != old.Yard.AccessLog {
		a, err := openAccessLog(cfg.Yard.AccessLog)
		if err != nil {
			return nil, fmt.Errorf("access log: %w", err)
		}
		access = a
	}

	s.mu.Lock()
	s.cfg = cfg
	if access != nil {
		s.access.Close()
		s.access = access
	}
	s.mu.Unlock()

	s.mgr.Apply(cfg.Servers)
	s.logger.Info("config applied", "servers", len(cfg.Servers))
	return notices, nil
}

// Handler returns the HTTP handler for all of yard.
func (s *Server) Handler() http.Handler {
	admin := s.adminHandler()
	return s.logAccess(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Paths containing ".." etc. make it ambiguous which server they belong to, so normalize them first.
		if r.Method != http.MethodConnect {
			if p := cleanPath(r.URL.Path); p != r.URL.Path {
				u := *r.URL
				u.Path, u.RawPath = p, ""
				http.Redirect(w, r, u.String(), http.StatusMovedPermanently)
				return
			}
		}
		if r.URL.Path == config.AdminPath || strings.HasPrefix(r.URL.Path, config.AdminPath+"/") {
			admin.ServeHTTP(w, r)
			return
		}
		s.proxy(w, r)
	}))
}

// cleanPath normalizes a path using the same rules as net/http.ServeMux.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	if p[len(p)-1] == '/' && np != "/" {
		np += "/"
	}
	return np
}

// writeFileAtomic writes to a temporary file and then replaces the target.
func writeFileAtomic(name string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), filepath.Base(name)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), name)
}

// waitReady waits for a starting server while the request is alive, up to its start timeout.
func waitReady(ctx context.Context, p *proc.Proc) string {
	ctx, cancel := context.WithTimeout(ctx, p.Config().StartTimeout.Duration)
	defer cancel()
	return p.WaitReady(ctx)
}
