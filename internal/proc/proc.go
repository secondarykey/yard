// Package proc は yard が起動するツールのプロセスを管理する。
//
// 1つのツールにつき1つの監視 goroutine（Proc.supervise）が、
// 空きポートの確保 → 起動 → ポートが開くのを待つ → 終了を待つ → 再起動
// を繰り返す。ポートは起動のたびに取り直す（前回のポートが TIME_WAIT や
// 他プロセスに取られていても起動できるように）。
//
// ツールが子プロセスを作っても取り残さないよう、停止はプロセスツリー単位で行う
// （Windows はジョブオブジェクト、それ以外はプロセスグループ。kill_*.go）。
package proc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"yard/internal/config"
)

// State はツールの状態。
type State string

const (
	StateStopped  State = "stopped"  // 停止している（停止操作または正常終了）
	StateStarting State = "starting" // 起動してポートが開くのを待っている
	StateRunning  State = "running"  // ポートが開いて転送できる
	StateBackoff  State = "backoff"  // 異常終了して再起動を待っている
	StateFailed   State = "failed"   // 異常終了し、再起動しない
	StateDisabled State = "disabled" // 設定で無効化されている
)

const (
	minBackoff  = 1 * time.Second
	maxBackoff  = 1 * time.Minute
	stopTimeout = 5 * time.Second // 穏当な停止を待つ時間。過ぎたら強制終了する
	// 起動後この時間以上動いていれば、次の異常終了での待ち時間を最短に戻す。
	stableAfter = 30 * time.Second
)

// Status は外部に見せるツールの状態。
type Status struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	State     State     `json:"state"`
	PID       int       `json:"pid,omitempty"`
	Port      int       `json:"port,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
	Restarts  int       `json:"restarts"`
	ExitCode  *int      `json:"exit_code,omitempty"`
	Error     string    `json:"error,omitempty"`
	Log       bool      `json:"log"`
}

// Proc は1つのツールのプロセス。
type Proc struct {
	tool   config.Tool
	logger *log.Logger

	mu        sync.Mutex
	state     State
	pid       int
	port      int
	startedAt time.Time
	restarts  int
	exitCode  *int
	lastErr   string
	ready     chan struct{} // starting の間だけ開いている。setStateLocked が管理する
	cancel    context.CancelFunc
	done      chan struct{} // 監視 goroutine が終わったら close される
}

// New は Proc を作る。起動はしない。
func New(t config.Tool, logger *log.Logger) *Proc {
	p := &Proc{tool: t, logger: logger, state: StateStopped, ready: make(chan struct{})}
	if t.Disabled {
		p.state = StateDisabled
	}
	return p
}

// Tool は設定を返す。
func (p *Proc) Tool() config.Tool { return p.tool }

// Status は現在の状態を返す。
func (p *Proc) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := Status{
		Name:     p.tool.Name,
		Path:     p.tool.Path,
		State:    p.state,
		Restarts: p.restarts,
		ExitCode: p.exitCode,
		Error:    p.lastErr,
		Log:      p.tool.Log != "",
	}
	if p.state == StateStarting || p.state == StateRunning {
		s.PID, s.Port, s.StartedAt = p.pid, p.port, p.startedAt
	}
	return s
}

// Target は転送先の "127.0.0.1:port" を返す。動いていなければ空。
func (p *Proc) Target() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != StateRunning {
		return ""
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(p.port))
}

// WaitReady は起動中のツールが動き出すのを ctx が終わるまで待ち、転送先を返す。
// 起動中でなければ待たずに Target を返す。
func (p *Proc) WaitReady(ctx context.Context) string {
	p.mu.Lock()
	starting := p.state == StateStarting
	ready := p.ready
	p.mu.Unlock()
	if starting {
		select {
		case <-ready:
		case <-ctx.Done():
		}
	}
	return p.Target()
}

// Start は監視を始める。既に動いていれば何もしない。
func (p *Proc) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tool.Disabled {
		return errors.New("disabled in config")
	}
	if p.done != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	p.restarts = 0
	p.lastErr = ""
	p.exitCode = nil
	// goroutine が動き出す前に来たリクエストも起動を待てるよう、ここで starting にする。
	p.setStateLocked(StateStarting)
	go p.supervise(ctx, p.done)
	return nil
}

// Stop はプロセスを止め、監視が終わるまで待つ。
func (p *Proc) Stop() {
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.mu.Unlock()
	if done == nil {
		return
	}
	cancel()
	<-done
}

// Restart は止めてから起動し直す。
func (p *Proc) Restart() error {
	p.Stop()
	return p.Start()
}

func (p *Proc) setState(s State) {
	p.mu.Lock()
	p.setStateLocked(s)
	p.mu.Unlock()
}

// setStateLocked は状態を変える。starting に入るときに ready を作り直し、
// starting から出るときに close して、WaitReady で待っている転送を起こす。
func (p *Proc) setStateLocked(s State) {
	if s == StateStarting && p.state != StateStarting {
		p.ready = make(chan struct{})
	}
	if s != StateStarting && p.state == StateStarting {
		close(p.ready)
	}
	p.state = s
}

// supervise は起動と再起動を繰り返す。ctx が終わると子プロセスを止めて戻る。
func (p *Proc) supervise(ctx context.Context, done chan struct{}) {
	defer func() {
		p.mu.Lock()
		if p.state != StateFailed {
			p.setStateLocked(StateStopped)
		}
		p.pid, p.port = 0, 0
		p.done, p.cancel = nil, nil
		p.mu.Unlock()
		close(done)
	}()

	backoff := minBackoff
	for {
		started := time.Now()
		code, err := p.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}

		p.mu.Lock()
		p.exitCode = code
		if err != nil {
			p.lastErr = err.Error()
		}
		p.mu.Unlock()

		failed := err != nil || code == nil || *code != 0
		switch {
		case p.tool.Restart == config.RestartNever,
			p.tool.Restart == config.RestartOnFailure && !failed:
			if failed {
				p.logger.Printf("[%s] exited: %v (no restart)", p.tool.Name, err)
				p.setState(StateFailed)
			} else {
				p.logger.Printf("[%s] exited normally", p.tool.Name)
			}
			return
		}

		if err == nil {
			err = errors.New("exit status 0")
		}
		if time.Since(started) > stableAfter {
			backoff = minBackoff
		}
		p.logger.Printf("[%s] exited: %v; restarting in %s", p.tool.Name, err, backoff)
		p.setState(StateBackoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
		p.mu.Lock()
		p.restarts++
		p.setStateLocked(StateStarting)
		p.mu.Unlock()
	}
}

// runOnce は1回起動して終了まで待つ。終了コードと理由を返す。
func (p *Proc) runOnce(ctx context.Context) (exitCode *int, err error) {
	port, err := FreePort()
	if err != nil {
		return nil, fmt.Errorf("allocate port: %w", err)
	}

	cmd, out, err := p.command(port)
	if err != nil {
		return nil, err
	}
	if out != nil {
		defer out.Close()
	}

	p.mu.Lock()
	p.port, p.pid = port, 0
	p.mu.Unlock()

	tree, err := startTree(cmd)
	if err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	defer tree.Close()

	p.mu.Lock()
	p.pid = cmd.Process.Pid
	p.startedAt = time.Now()
	p.mu.Unlock()
	p.logger.Printf("[%s] started pid=%d port=%d", p.tool.Name, cmd.Process.Pid, port)
	if out != nil {
		fmt.Fprintf(out, "\n=== yard: %s started pid=%d port=%d at %s ===\n",
			p.tool.Name, cmd.Process.Pid, port, time.Now().Format(time.RFC3339))
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// ポートが開くまで待つ。
	readyCh := make(chan bool, 1)
	waitCtx, stopWait := context.WithTimeout(ctx, p.tool.StartTimeout.Duration)
	defer stopWait()
	go func() { readyCh <- waitPort(waitCtx, port) }()

	var waitErr error
	select {
	case waitErr = <-exited:
		stopWait()
		return exitStatus(cmd, waitErr)
	case ok := <-readyCh:
		if ctx.Err() != nil {
			return p.terminate(tree, exited)
		}
		if !ok {
			p.logger.Printf("[%s] port %d did not open within %s", p.tool.Name, port, p.tool.StartTimeout)
			p.terminate(tree, exited)
			return nil, fmt.Errorf("port %d did not open within %s (does the tool listen on {port}?)", port, p.tool.StartTimeout)
		}
	}

	p.mu.Lock()
	p.setStateLocked(StateRunning)
	p.lastErr = ""
	p.mu.Unlock()
	p.logger.Printf("[%s] ready on port %d", p.tool.Name, port)

	select {
	case waitErr = <-exited:
		return exitStatus(cmd, waitErr)
	case <-ctx.Done():
		return p.terminate(tree, exited)
	}
}

// terminate は穏当に止め、時間内に終わらなければ強制終了する。
func (p *Proc) terminate(tree *processTree, exited <-chan error) (*int, error) {
	p.logger.Printf("[%s] stopping", p.tool.Name)
	if err := tree.Interrupt(); err == nil {
		select {
		case <-exited:
			return nil, nil
		case <-time.After(stopTimeout):
		}
	}
	if err := tree.Kill(); err != nil {
		p.logger.Printf("[%s] kill: %v", p.tool.Name, err)
	}
	select {
	case <-exited:
	case <-time.After(stopTimeout):
		p.logger.Printf("[%s] did not exit after kill", p.tool.Name)
	}
	return nil, nil
}

func exitStatus(cmd *exec.Cmd, err error) (*int, error) {
	if cmd.ProcessState == nil {
		return nil, err
	}
	code := cmd.ProcessState.ExitCode()
	if code == 0 {
		return &code, nil
	}
	return &code, fmt.Errorf("exit status %d", code)
}

// command は置換済みの引数・環境変数で exec.Cmd を作る。
// 出力先ファイルを開いた場合はそれも返す（呼び出し側で閉じる）。
func (p *Proc) command(port int) (*exec.Cmd, io.WriteCloser, error) {
	t := p.tool
	r := Replacer(t, port)

	args := make([]string, len(t.Args))
	for i, a := range t.Args {
		args[i] = r.Replace(a)
	}
	cmd := exec.Command(t.Command, args...)
	if cmd.Err != nil {
		return nil, nil, cmd.Err
	}
	cmd.Dir = t.Dir

	env := os.Environ()
	env = append(env,
		"YARD_PORT="+strconv.Itoa(port),
		"YARD_BASE="+base(t.Path),
		"YARD_NAME="+t.Name,
	)
	for _, e := range t.Env {
		env = append(env, r.Replace(e))
	}
	cmd.Env = env

	var out io.WriteCloser
	if t.Log != "" {
		f, err := OpenLog(t.Log)
		if err != nil {
			return nil, nil, fmt.Errorf("open log: %w", err)
		}
		cmd.Stdout, cmd.Stderr = f, f
		out = f
	}
	// Stdout/Stderr が nil なら os.DevNull に繋がる（出力は捨てる）。
	return cmd, out, nil
}

// Replacer は args / env の置換子を返す。
func Replacer(t config.Tool, port int) *strings.Replacer {
	ps := strconv.Itoa(port)
	return strings.NewReplacer(
		"{port}", ps,
		"{base}", base(t.Path),
		"{path}", t.Path,
		"{name}", t.Name,
		"{addr}", net.JoinHostPort("127.0.0.1", ps),
	)
}

// base は公開パスを末尾スラッシュ付きで返す。
func base(p string) string {
	if strings.HasSuffix(p, "/") {
		return p
	}
	return p + "/"
}

// OpenLog は追記用にログファイルを開く。ディレクトリが無ければ作る。
func OpenLog(name string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// FreePort はループバックの空きポートを1つ返す。
// 閉じてから子プロセスが使うまでの間に取られる可能性はあるが、
// その場合は子が起動に失敗し、再起動時に別のポートを取り直す。
func FreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// waitPort はポートに接続できるまで待つ。ctx が終われば false。
func waitPort(ctx context.Context, port int) bool {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	d := net.Dialer{Timeout: 500 * time.Millisecond}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if c, err := d.DialContext(ctx, "tcp", addr); err == nil {
			c.Close()
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
	}
}
