// Package proc manages the processes of the servers that yard starts.
//
// Each server has one supervising goroutine (Proc.supervise) that repeats
// allocate a free port → start → wait for the port to open → wait for exit → restart.
// A new port is picked on every start (so that it can start even if the previous port
// is in TIME_WAIT or taken by another process).
//
// Stopping works on the whole process tree so that child processes created by a server
// are not left behind (a job object on Windows, a process group elsewhere; tree_*.go).
package proc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// State is the state of a server.
type State string

const (
	StateStopped  State = "stopped"  // stopped (by a stop request or normal exit)
	StateStarting State = "starting" // started and waiting for the port to open
	StateRunning  State = "running"  // port is open and requests can be forwarded
	StateBackoff  State = "backoff"  // exited abnormally and waiting to restart
	StateFailed   State = "failed"   // exited abnormally and will not restart
	StateDisabled State = "disabled" // disabled in the configuration
)

const (
	minBackoff  = 1 * time.Second
	maxBackoff  = 1 * time.Minute
	stopTimeout = 5 * time.Second // how long to wait for a graceful stop before killing
	// If it ran at least this long, the wait before the next restart goes back to the minimum.
	stableAfter = 30 * time.Second
)

// Status is the externally visible state of a server.
type Status struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	State     State     `json:"state"`
	PID       int       `json:"pid,omitempty"`
	Port      int       `json:"port,omitempty"`
	Direct    string    `json:"direct,omitempty"` // URL of the server itself, bypassing yard (reachable from this machine only)
	StartedAt time.Time `json:"started_at,omitzero"`
	Restarts  int       `json:"restarts"`
	ExitCode  *int      `json:"exit_code,omitempty"`
	Error     string    `json:"error,omitempty"`
	Log       bool      `json:"log"`     // log is on in the configuration
	HasLog    bool      `json:"has_log"` // there is a log file to show (LogFile)
	LocalOnly bool      `json:"local_only"`
}

// Proc is the process of one server.
type Proc struct {
	conf   config.Server
	logger *slog.Logger // with server=<name>

	mu        sync.Mutex
	state     State
	pid       int
	port      int
	startedAt time.Time
	restarts  int
	exitCode  *int
	lastErr   string
	logFile   string        // output file of the current or last start; kept after it stops
	ready     chan struct{} // open only while starting; managed by setStateLocked
	cancel    context.CancelFunc
	done      chan struct{} // closed when the supervising goroutine finishes
}

// New creates a Proc without starting it.
func New(t config.Server, logger *slog.Logger) *Proc {
	p := &Proc{conf: t, logger: logger.With("server", t.Name), state: StateStopped, ready: make(chan struct{})}
	if t.Disabled {
		p.state = StateDisabled
	}
	return p
}

// Config returns the configuration.
func (p *Proc) Config() config.Server { return p.conf }

// Status returns the current state.
func (p *Proc) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := Status{
		Name:      p.conf.Name,
		Path:      p.conf.Path,
		State:     p.state,
		Restarts:  p.restarts,
		ExitCode:  p.exitCode,
		Error:     p.lastErr,
		Log:       p.conf.Log,
		HasLog:    p.logFile != "",
		LocalOnly: p.conf.LocalOnly,
	}
	if p.state == StateStarting || p.state == StateRunning {
		s.PID, s.Port, s.StartedAt = p.pid, p.port, p.startedAt
		s.Direct = DirectURL(p.conf, p.port)
	}
	return s
}

// DirectURL returns the URL of the server itself on port: the address yard forwards to, and the path
// the server serves under ({base}, or "/" with strip_prefix).
func DirectURL(t config.Server, port int) string {
	scheme, p := "http", base(t.Path)
	if t.HTTPS {
		scheme = "https"
	}
	if t.StripPrefix {
		p = "/"
	}
	return scheme + "://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + p
}

// LogFile returns the output file of the current or last start ({name}-{pid}.log),
// or "" if nothing has been written yet.
func (p *Proc) LogFile() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.logFile
}

// Target returns the forwarding target "127.0.0.1:port", or "" if not running.
func (p *Proc) Target() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != StateRunning {
		return ""
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(p.port))
}

// WaitReady waits (until ctx is done) for a starting server to become ready and returns the target.
// If it is not starting, it returns Target without waiting.
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

// Start begins supervising. It does nothing if already running.
func (p *Proc) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conf.Disabled {
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
	// Set starting here so that requests arriving before the goroutine runs also wait for startup.
	p.setStateLocked(StateStarting)
	go p.supervise(ctx, p.done)
	return nil
}

// Stop stops the process and waits for supervision to finish.
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

// Restart stops and then starts again.
func (p *Proc) Restart() error {
	p.Stop()
	return p.Start()
}

func (p *Proc) setState(s State) {
	p.mu.Lock()
	p.setStateLocked(s)
	p.mu.Unlock()
}

// setStateLocked changes the state. It recreates ready when entering starting and
// closes it when leaving starting, waking up requests waiting in WaitReady.
func (p *Proc) setStateLocked(s State) {
	if s == StateStarting && p.state != StateStarting {
		p.ready = make(chan struct{})
	}
	if s != StateStarting && p.state == StateStarting {
		close(p.ready)
	}
	p.state = s
}

// supervise starts and restarts repeatedly. When ctx is done it stops the child process and returns.
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
		case p.conf.Restart == config.RestartNever,
			p.conf.Restart == config.RestartOnFailure && !failed:
			if failed {
				p.logger.Warn("exited (no restart)", "err", err)
				p.setState(StateFailed)
			} else {
				p.logger.Info("exited normally")
			}
			return
		}

		if err == nil {
			err = errors.New("exit status 0")
		}
		if time.Since(started) > stableAfter {
			backoff = minBackoff
		}
		p.logger.Warn("exited; restarting", "err", err, "after", backoff)
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

// runOnce starts once and waits for exit. It returns the exit code and the reason.
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
		// Written before the start so that it comes before the server's own output. The pid goes into the name.
		fmt.Fprintf(out, "=== yard: %s starting port=%d at %s ===\n",
			p.conf.Name, port, time.Now().Format(time.RFC3339))
	}

	p.mu.Lock()
	p.port, p.pid = port, 0
	p.mu.Unlock()

	tree, err := startTree(cmd)
	if err != nil {
		if out != nil {
			out.Close()
			os.Remove(out.Name())
		}
		return nil, fmt.Errorf("start: %w", err)
	}
	defer tree.Close()

	pid := cmd.Process.Pid
	logFile := ""
	if out != nil {
		logFile = p.nameLog(out.Name(), pid)
	}
	p.mu.Lock()
	p.pid = pid
	p.startedAt = time.Now()
	if logFile != "" {
		p.logFile = logFile
	}
	p.mu.Unlock()
	p.logger.Info("started", "pid", pid, "port", port)

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// Wait for the port to open.
	readyCh := make(chan bool, 1)
	waitCtx, stopWait := context.WithTimeout(ctx, p.conf.StartTimeout.Duration)
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
			p.logger.Warn("port did not open in time", "port", port, "timeout", p.conf.StartTimeout)
			p.terminate(tree, exited)
			return nil, fmt.Errorf("port %d did not open within %s (does the server listen on {port}?)", port, p.conf.StartTimeout)
		}
	}

	p.mu.Lock()
	p.setStateLocked(StateRunning)
	p.lastErr = ""
	p.mu.Unlock()
	p.logger.Info("ready", "port", port)

	select {
	case waitErr = <-exited:
		return exitStatus(cmd, waitErr)
	case <-ctx.Done():
		return p.terminate(tree, exited)
	}
}

// terminate stops gracefully, and kills if it does not exit in time.
func (p *Proc) terminate(tree *processTree, exited <-chan error) (*int, error) {
	p.logger.Info("stopping")
	if err := tree.Interrupt(); err == nil {
		select {
		case <-exited:
			return nil, nil
		case <-time.After(stopTimeout):
		}
	}
	if err := tree.Kill(); err != nil {
		p.logger.Error("kill", "err", err)
	}
	select {
	case <-exited:
	case <-time.After(stopTimeout):
		p.logger.Error("did not exit after kill")
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

// command builds an exec.Cmd with substituted arguments and environment.
// If it opened an output file, that is returned too (the caller closes it). The file has a
// temporary name until the process has a pid (nameLog).
func (p *Proc) command(port int) (*exec.Cmd, *os.File, error) {
	t := p.conf
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

	var out *os.File
	if t.Log {
		f, err := openLog(filepath.Join(t.LogDir, t.Name+"-starting.log"))
		if err != nil {
			return nil, nil, fmt.Errorf("open log: %w", err)
		}
		cmd.Stdout, cmd.Stderr = f, f
		out = f
	}
	// If Stdout/Stderr are nil they are connected to os.DevNull (output is discarded).
	return cmd, out, nil
}

// nameLog renames the output file to {name}-{pid}.log now that the process has a pid, and returns the
// name it ends up with. The server already writes to it; openLog lets it be renamed while open.
func (p *Proc) nameLog(tmp string, pid int) string {
	name := filepath.Join(filepath.Dir(tmp), fmt.Sprintf("%s-%d.log", p.conf.Name, pid))
	if err := os.Rename(tmp, name); err != nil {
		p.logger.Warn("rename log", "err", err)
		return tmp
	}
	return name
}

// Replacer returns the substitution replacer for args / env.
func Replacer(t config.Server, port int) *strings.Replacer {
	ps := strconv.Itoa(port)
	return strings.NewReplacer(
		"{port}", ps,
		"{base}", base(t.Path),
		"{path}", t.Path,
		"{name}", t.Name,
		"{addr}", net.JoinHostPort("127.0.0.1", ps),
	)
}

// base returns the public path with a trailing slash.
func base(p string) string {
	if strings.HasSuffix(p, "/") {
		return p
	}
	return p + "/"
}

// FreePort returns one free loopback port.
// It may be taken between closing it and the child using it; in that case the child
// fails to start and a different port is picked on restart.
func FreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// waitPort waits until the port accepts connections. It returns false when ctx is done.
// It tries often at first and less often later, so that a server that starts quickly
// is noticed quickly (requests that arrived meanwhile are waiting for it).
func waitPort(ctx context.Context, port int) bool {
	const (
		firstWait = 10 * time.Millisecond
		maxWait   = 100 * time.Millisecond
	)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	d := net.Dialer{Timeout: 500 * time.Millisecond}
	timer := time.NewTimer(firstWait)
	defer timer.Stop()
	for wait := firstWait; ; wait = min(wait*2, maxWait) {
		if c, err := d.DialContext(ctx, "tcp", addr); err == nil {
			c.Close()
			return true
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
		}
	}
}
