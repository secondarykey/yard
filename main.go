// yard はイントラ内でツール群を公開するためのプロキシサーバ。
//
// 設定ファイル（yard.ini）に書いたプログラムを空きポートで起動し、
// 指定した URI 以下へのリクエストをそのプログラムへ転送する。
// /.yard が yard 自身の管理画面になる。
//
//	yard                前面で起動する
//	yard init           設定ファイルの雛形を書き出す
//	yard install        サービスとして登録する
//	yard start / stop / restart / status / uninstall
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kardianos/service"

	"yard/internal/config"
	"yard/internal/proc"
	"yard/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "yard: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfgFlag := flag.String("config", "", "path to yard.ini (default: ./yard.ini, or next to the executable)")
	flag.Usage = usage
	flag.Parse()

	cfgPath := *cfgFlag
	if cfgPath == "" {
		cfgPath = config.DefaultPath()
	}
	cfgPath, err := filepath.Abs(cfgPath)
	if err != nil {
		return err
	}

	cmd := flag.Arg(0)
	if cmd == "init" {
		return writeTemplate(cfgPath)
	}

	cfg, err := config.Load(cfgPath)
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
		// サービスの作業ディレクトリは System32 や / になるため、
		// 設定ファイルは絶対パスで渡しておく。
		Arguments: []string{"-config", cfgPath},
		Option: service.KeyValue{
			// systemd / launchd などで異常終了時に再起動させる。
			"Restart":     "on-failure",
			"KeepAlive":   true,
			"RunAtLoad":   true,
			"UserService": cfg.Service.UserService,
			// Windows: SCM の回復オプション（異常終了時に再起動）。
			"OnFailure":              "restart",
			"OnFailureDelayDuration": "5s",
		},
	}
	svc, err := service.New(prg, svcCfg)
	if err != nil {
		return err
	}

	if cmd != "" {
		return control(svc, cmd, cfgPath)
	}

	logger, closeLog, err := newLogger(svc, cfg)
	if err != nil {
		return err
	}
	defer closeLog()
	prg.logger = logger
	return svc.Run()
}

// control は install/start/stop などのサブコマンドを処理する。
func control(svc service.Service, cmd, cfgPath string) error {
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
		fmt.Printf("installed (config: %s)\n", cfgPath)
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

// stopForUninstall は、動いているサービスを止めてから戻る。
//
// kardianos/service の Uninstall は Windows と systemd では停止しない。
// Windows では「削除予定」のまま動き続け、systemd では登録が消えたプロセスが
// ツールごと残る。止められなかったときは登録も外さずにエラーにする。
func stopForUninstall(svc service.Service) error {
	st, err := svc.Status()
	if err != nil || st != service.StatusRunning {
		// 未登録などで状態が取れない場合は、Uninstall 側のエラーに任せる。
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
	if err := os.WriteFile(path, []byte(config.Template), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", path)
	return nil
}

// newLogger は yard 自身のログの出力先を決める。
//
//   - server.log があればそのファイルへ（前面起動時は標準エラーにも）。
//   - 無ければ、前面起動時は標準エラー、サービス時は OS のログ
//     （Windows はイベントログ、Linux は syslog/journald）へ。
func newLogger(svc service.Service, cfg *config.Config) (*log.Logger, func(), error) {
	var w io.Writer = os.Stderr
	closer := func() {}
	if !service.Interactive() {
		if sl, err := svc.Logger(nil); err == nil {
			w = serviceWriter{sl}
		}
	}
	flags := log.LstdFlags
	if cfg.Server.Log != "" {
		f, err := proc.OpenLog(cfg.Server.Log)
		if err != nil {
			return nil, nil, fmt.Errorf("log: %w", err)
		}
		closer = func() { f.Close() }
		if service.Interactive() {
			w = io.MultiWriter(os.Stderr, f)
		} else {
			w = f
		}
	} else if !service.Interactive() {
		flags = 0 // OS のログが時刻を付ける
	}
	return log.New(w, "", flags), closer, nil
}

type serviceWriter struct{ l service.Logger }

func (w serviceWriter) Write(p []byte) (int, error) {
	return len(p), w.l.Info(strings.TrimRight(string(p), "\n"))
}

// program は service.Interface を満たす。
type program struct {
	cfgPath string
	cfg     *config.Config
	logger  *log.Logger

	srv  *server.Server
	http *http.Server
}

// Start はブロックしてはいけない。listen までを同期で行い、
// ポート競合などの起動失敗をサービスマネージャへ伝える。
func (p *program) Start(service.Service) error {
	ln, err := net.Listen("tcp", p.cfg.Server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", p.cfg.Server.Addr, err)
	}

	srv, err := server.New(p.cfgPath, p.cfg, p.logger)
	if err != nil {
		ln.Close()
		return err
	}
	p.srv = srv
	p.http = &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          p.logger,
		// 本文の読み書きには期限を設けない。ツール側の長時間のレスポンス
		// （SSE、WebSocket、大きなダウンロード）を途中で切らないため。
	}
	p.logger.Printf("listening on %s (config: %s, %d tools)", ln.Addr(), p.cfgPath, len(p.cfg.Tools))
	go func() {
		if err := p.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.logger.Printf("serve: %v", err)
		}
	}()
	return nil
}

// Stop は素早く返す必要がある。Windows の SCM の停止待ちは既定で 20 秒程度。
func (p *program) Stop(service.Service) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if p.http != nil {
		if err := p.http.Shutdown(ctx); err != nil {
			p.http.Close()
		}
	}
	if p.srv != nil {
		p.srv.Close()
	}
	p.logger.Printf("stopped")
	return nil
}

func usage() {
	fmt.Fprintf(flag.CommandLine.Output(), `yard - ツール群をイントラ内で公開するプロキシサーバ

usage:
  yard [flags]              前面で起動する
  yard [flags] <command>    サービスを操作する

commands:
  init        設定ファイルの雛形を書き出す
  install     サービスとして登録する（-config の絶対パスが登録される）
  uninstall   動いていれば停止し、登録を解除する
  start       開始
  stop        停止
  restart     再起動
  status      状態を表示する

flags:
`)
	flag.PrintDefaults()
}
