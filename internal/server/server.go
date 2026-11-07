// Package server は yard の HTTP 側。ツールへの転送（proxy.go）、
// アクセスログ（accesslog.go）、管理画面 /.yard（admin.go）を持つ。
//
// 転送ヘッダ（X-Forwarded-For など）は接続元の判定に使わない。
// 詐称で /.yard の接続元制限を抜けられるため。判定は RemoteAddr のみで行う。
package server

import (
	"bytes"
	"context"
	"fmt"
	"log"
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

// Server は設定・プロセス管理・HTTP ハンドラを束ねる。
type Server struct {
	path   string // 設定ファイルの絶対パス
	logger *log.Logger
	mgr    *proc.Manager

	transport *http.Transport

	mu     sync.RWMutex
	cfg    *config.Config
	access *accessLog
	addr   string // 起動時の server.addr。再読み込みでは変えられない
	saveMu sync.Mutex
}

// New は設定を反映し、ツールを起動する。
func New(cfgPath string, cfg *config.Config, logger *log.Logger) (*Server, error) {
	access, err := openAccessLog(cfg.Server.AccessLog)
	if err != nil {
		return nil, fmt.Errorf("access log: %w", err)
	}
	s := &Server{
		path:   cfgPath,
		logger: logger,
		mgr:    proc.NewManager(logger),
		cfg:    cfg,
		access: access,
		addr:   cfg.Server.Addr,
		transport: &http.Transport{
			Proxy: nil, // 転送先はループバックなので、環境変数のプロキシ設定を使わない
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
	s.mgr.Apply(cfg.Tools)
	return s, nil
}

// Config は現在の設定を返す。
func (s *Server) Config() *config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Manager はプロセス管理を返す。
func (s *Server) Manager() *proc.Manager { return s.mgr }

// Close は全ツールを止め、ログを閉じる。
func (s *Server) Close() {
	s.mgr.StopAll()
	s.transport.CloseIdleConnections()
	s.mu.Lock()
	s.access.Close()
	s.access = nil
	s.mu.Unlock()
}

// Reload は設定ファイルを読み直して反映する。
// 戻り値の notices は、反映できなかった（再起動が必要な）項目の説明。
func (s *Server) Reload() (notices []string, err error) {
	cfg, err := config.Load(s.path)
	if err != nil {
		return nil, err
	}
	return s.apply(cfg)
}

// ReadConfig は設定ファイルの本文を返す。
func (s *Server) ReadConfig() ([]byte, error) {
	return os.ReadFile(s.path)
}

// SaveConfig は本文を検証してから設定ファイルに書き、反映する。
// 検証に失敗した場合はファイルを変更しない。
func (s *Server) SaveConfig(data []byte) (notices []string, err error) {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	cfg, err := config.Parse(data, filepath.Dir(s.path))
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(s.path, data); err != nil {
		return nil, err
	}
	s.logger.Printf("config saved: %s", s.path)
	return s.apply(cfg)
}

func (s *Server) apply(cfg *config.Config) ([]string, error) {
	var notices []string
	old := s.Config()
	if cfg.Server.Addr != s.addr {
		notices = append(notices, fmt.Sprintf("server.addr (%s → %s) は yard の再起動後に反映されます", s.addr, cfg.Server.Addr))
	}
	if cfg.Server.Log != old.Server.Log {
		notices = append(notices, "server.log は yard の再起動後に反映されます")
	}
	if cfg.Service != old.Service {
		notices = append(notices, "[service] はサービスの再登録（uninstall → install）で反映されます")
	}

	var access *accessLog
	if cfg.Server.AccessLog != old.Server.AccessLog {
		a, err := openAccessLog(cfg.Server.AccessLog)
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

	s.mgr.Apply(cfg.Tools)
	s.logger.Printf("config applied (%d tools)", len(cfg.Tools))
	return notices, nil
}

// Handler は yard 全体の HTTP ハンドラを返す。
func (s *Server) Handler() http.Handler {
	admin := s.adminHandler()
	return s.logAccess(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ".." などを含むパスは、どのツールに属するかが曖昧になるので正規化してから扱う。
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

// cleanPath は net/http.ServeMux と同じ規則でパスを正規化する。
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

// writeFileAtomic は一時ファイルに書いてから置き換える。
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

// waitReady は起動中のツールを、リクエストが続く間・起動待ち時間の範囲で待つ。
func waitReady(ctx context.Context, p *proc.Proc) string {
	ctx, cancel := context.WithTimeout(ctx, p.Tool().StartTimeout.Duration)
	defer cancel()
	return p.WaitReady(ctx)
}
