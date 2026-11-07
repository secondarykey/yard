// yard is a proxy server for publishing a set of servers inside an intranet.
//
// It starts the programs listed in the configuration file (yard.ini) on free ports
// and forwards requests under the given URIs to those programs.
// /.yard is yard's own admin UI.
//
//	yard                run in the foreground
//	yard init           write a template configuration file
//	yard install        register as a service
//	yard start / stop / restart / status / uninstall
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kardianos/service"
	wlog "github.com/wenteasy/log"

	"yard/internal/config"
	"yard/internal/logfile"
	"yard/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "yard: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfgFlag := flag.String("config", config.DefaultPath(), "path to yard.ini")
	logFlag := flag.String("logdir", config.DefaultLogDir(), "directory for log files")
	flag.Usage = usage
	flag.Parse()

	cfgPath, err := filepath.Abs(*cfgFlag)
	if err != nil {
		return err
	}
	logDir, err := filepath.Abs(*logFlag)
	if err != nil {
		return err
	}

	cmd := flag.Arg(0)
	if cmd == "init" {
		return writeTemplate(cfgPath)
	}

	cfg, err := config.Load(cfgPath, logDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s not found (run `yard init` to create one)", cfgPath)
		}
		return fmt.Errorf("%s: %w", cfgPath, err)
	}

	prg := &program{cfgPath: cfgPath, cfg: cfg}
	svcCfg := &service.Config{
		Name:        cfg.Service.Name,
		DisplayName: cfg.Service.DisplayName,
		Description: cfg.Service.Description,
		UserName:    cfg.Service.User,
		// A service's working directory is System32 or /, so pass the
		// configuration file as an absolute path. The log location is also pinned
		// to %TEMP%\yard of the installing user (LocalSystem's %TEMP% is the system one).
		Arguments: []string{"-config", cfgPath, "-logdir", logDir},
		Option: service.KeyValue{
			// Restart on abnormal exit under systemd / launchd etc.
			"Restart":     "on-failure",
			"KeepAlive":   true,
			"RunAtLoad":   true,
			"UserService": cfg.Service.UserService,
			// Windows: SCM recovery options (restart on abnormal exit).
			"OnFailure":              "restart",
			"OnFailureDelayDuration": "5s",
		},
	}
	svc, err := service.New(prg, svcCfg)
	if err != nil {
		return err
	}

	if cmd != "" {
		return control(svc, cmd, cfgPath, logDir)
	}

	logger, closeLog, err := newLogger(svc, cfg)
	if err != nil {
		return err
	}
	defer closeLog()
	prg.logger = logger
	slog.SetDefault(logger)
	return svc.Run()
}

// control handles subcommands such as install/start/stop.
func control(svc service.Service, cmd, cfgPath, logDir string) error {
	switch cmd {
	case "status":
		st, err := svc.Status()
		if err != nil {
			return err
		}
		fmt.Println(statusName(st))
		return nil
	case "install":
		if err := svc.Install(); err != nil {
			return err
		}
		fmt.Printf("installed (config: %s, logdir: %s)\n", cfgPath, logDir)
		return nil
	case "uninstall":
		if err := stopForUninstall(svc); err != nil {
			return err
		}
		if err := svc.Uninstall(); err != nil {
			return fmt.Errorf("uninstall: %w", err)
		}
		fmt.Println("uninstall: ok")
		return nil
	}
	if err := service.Control(svc, cmd); err != nil {
		return fmt.Errorf("%s: %w (valid: init, status, %s)", cmd, err, strings.Join(service.ControlAction[:], ", "))
	}
	fmt.Printf("%s: ok\n", cmd)
	return nil
}

// stopForUninstall stops the service if it is running before returning.
//
// kardianos/service's Uninstall does not stop the service on Windows or systemd.
// On Windows it keeps running "marked for deletion", and on systemd the unregistered
// process is left running along with its servers. If it cannot be stopped,
// return an error without unregistering.
func stopForUninstall(svc service.Service) error {
	st, err := svc.Status()
	if err != nil || st != service.StatusRunning {
		// If the status is unavailable (e.g. not installed), leave it to Uninstall's error.
		return nil
	}
	fmt.Println("stopping...")
	if err := svc.Stop(); err != nil {
		return fmt.Errorf("stop before uninstall: %w", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		st, err := svc.Status()
		if err != nil || st != service.StatusRunning {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("stop before uninstall: service is still running")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func statusName(s service.Status) string {
	switch s {
	case service.StatusRunning:
		return "running"
	case service.StatusStopped:
		return "stopped"
	default:
		return "unknown (not installed?)"
	}
}

func writeTemplate(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(config.Template), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", path)
	return nil
}

// newLogger decides where yard's own log goes.
//
//   - If yard.log is on, to yard_YYYYMMDD.log in the log directory (and also to stderr when in the foreground).
//   - Otherwise to stderr in the foreground, or to the OS log when running as a service
//     (the Event Log on Windows, syslog/journald on Linux).
func newLogger(svc service.Service, cfg *config.Config) (*slog.Logger, func(), error) {
	var hs []slog.Handler
	closer := func() {}
	if name := cfg.YardLog(); name != "" {
		w, err := logfile.Daily(name, true)
		if err != nil {
			return nil, nil, fmt.Errorf("log: %w", err)
		}
		closer = func() { w.Close() }
		hs = append(hs, wlog.NewSimpleHandler(w, nil))
	}
	if service.Interactive() {
		hs = append(hs, wlog.NewSimpleHandler(os.Stderr, nil))
	} else if len(hs) == 0 {
		if sl, err := svc.Logger(nil); err == nil {
			hs = append(hs, newServiceHandler(sl))
		} else {
			hs = append(hs, wlog.NewSimpleHandler(os.Stderr, nil))
		}
	}
	return slog.New(slog.NewMultiHandler(hs...)), closer, nil
}

// serviceHandler sends records to the OS log, keeping their level
// (Warn → warning, Error and above → error). The OS log adds its own timestamps.
type serviceHandler struct {
	l     service.Logger
	mu    *sync.Mutex
	buf   *bytes.Buffer
	inner slog.Handler // formats into buf
}

func newServiceHandler(l service.Logger) slog.Handler {
	buf := new(bytes.Buffer)
	return &serviceHandler{l: l, mu: new(sync.Mutex), buf: buf, inner: wlog.NewSimpleHandler(buf, nil)}
}

func (h *serviceHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *serviceHandler) Handle(ctx context.Context, r slog.Record) error {
	r.Time = time.Time{} // the OS log has its own
	h.mu.Lock()
	defer h.mu.Unlock()
	h.buf.Reset()
	if err := h.inner.Handle(ctx, r); err != nil {
		return err
	}
	line := strings.TrimRight(h.buf.String(), "\n")
	if _, rest, ok := strings.Cut(line, "] "); ok { // drop "[INFO  ] "; the OS log has the level
		line = rest
	}
	switch {
	case r.Level >= slog.LevelError:
		return h.l.Error(line)
	case r.Level >= slog.LevelWarn:
		return h.l.Warning(line)
	default:
		return h.l.Info(line)
	}
}

func (h *serviceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := *h
	nh.inner = h.inner.WithAttrs(attrs)
	return &nh
}

func (h *serviceHandler) WithGroup(name string) slog.Handler {
	nh := *h
	nh.inner = h.inner.WithGroup(name)
	return &nh
}

// program implements service.Interface.
type program struct {
	cfgPath string
	cfg     *config.Config
	logger  *slog.Logger

	srv      *server.Server
	http     *http.Server
	redirect *http.Server // plain HTTP → https on yard.redirect_addr; nil if not used
}

// Start must not block. It does everything up to listen synchronously so that
// startup failures such as port conflicts are reported to the service manager.
func (p *program) Start(service.Service) error {
	ln, err := net.Listen("tcp", p.cfg.Yard.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", p.cfg.Yard.Addr, err)
	}

	var redirectLn net.Listener
	if a := p.cfg.Yard.RedirectAddr; a != "" {
		if redirectLn, err = net.Listen("tcp", a); err != nil {
			ln.Close()
			return fmt.Errorf("listen on %s (redirect_addr): %w", a, err)
		}
	}

	srv, err := server.New(p.cfgPath, p.cfg, p.logger)
	if err != nil {
		ln.Close()
		if redirectLn != nil {
			redirectLn.Close()
		}
		return err
	}
	p.srv = srv
	p.http = &http.Server{
		Handler:           srv.Handler(),
		TLSConfig:         srv.TLSConfig(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(p.logger.Handler(), slog.LevelWarn),
		// No deadlines on reading/writing bodies, so that long-running responses
		// from servers (SSE, WebSocket, large downloads) are not cut off.
	}
	scheme := "http"
	if p.http.TLSConfig != nil {
		scheme = "https"
	}
	p.logger.Info("listening", "addr", ln.Addr(), "scheme", scheme, "config", p.cfgPath, "logdir", p.cfg.LogDir, "servers", len(p.cfg.Servers))
	go func() {
		var err error
		if p.http.TLSConfig != nil {
			err = p.http.ServeTLS(ln, "", "") // the certificate comes from TLSConfig
		} else {
			err = p.http.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.logger.Error("serve", "err", err)
		}
	}()

	if redirectLn != nil {
		p.redirect = &http.Server{
			Handler:           server.RedirectHandler(p.cfg.Yard.Addr),
			ReadHeaderTimeout: 10 * time.Second,
			ErrorLog:          slog.NewLogLogger(p.logger.Handler(), slog.LevelWarn),
		}
		p.logger.Info("redirecting http to https", "addr", redirectLn.Addr())
		go func() {
			if err := p.redirect.Serve(redirectLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				p.logger.Error("serve redirect", "err", err)
			}
		}()
	}
	return nil
}

// Stop must return quickly. The Windows SCM waits about 20 seconds by default.
func (p *program) Stop(service.Service) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, h := range []*http.Server{p.redirect, p.http} {
		if h != nil {
			if err := h.Shutdown(ctx); err != nil {
				h.Close()
			}
		}
	}
	if p.srv != nil {
		p.srv.Close()
	}
	p.logger.Info("stopped")
	return nil
}

func usage() {
	fmt.Fprintf(flag.CommandLine.Output(), `yard - a proxy server for publishing servers inside an intranet

usage:
  yard [flags]              run in the foreground
  yard [flags] <command>    control the service

commands:
  init        write a template configuration file
  install     register as a service (absolute -config / -logdir paths are registered)
  uninstall   stop if running, then unregister
  start       start the service
  stop        stop the service
  restart     restart the service
  status      show the service status

flags:
`)
	flag.PrintDefaults()
}
