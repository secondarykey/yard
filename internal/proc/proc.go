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
	StartedAt time.Time `json:"started_at,omitzero"`
	Restarts  int       `json:"restarts"`
	ExitCode  *int      `json:"exit_code,omitempty"`
	Error     string    `json:"error,omitempty"`
	Log       bool      `json:"log"`
}

// Proc is the process of one server.
type Proc struct {
	conf   config.Server
	logger *log.Logger

	mu        sync.Mutex
	state     State
	pid       int
	port      int
	startedAt time.Time
	restarts  int
	exitCode  *int
	lastErr   string
	ready     chan struct{} // open only while starting; managed by setStateLocked
	cancel    context.CancelFunc
	done      chan struct{} // closed when the supervising goroutine finishes
}

// New creates a Proc without starting it.
func New(t config.Server, logger *log.Logger) *Proc {
	p := &Proc{conf: t, logger: logger, state: StateStopped, ready: make(chan struct{})}
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
		Name:     p.conf.Name,
		Path:     p.conf.Path,
		State:    p.state,
		Restarts: p.restarts,
		ExitCode: p.exitCode,
		Error:    p.lastErr,
		Log:      p.conf.Log != "",
	}
	if p.state == StateStarting || p.state == StateRunning {
		s.PID, s.Port, s.StartedAt = p.pid, p.port, p.startedAt
	}
	return s
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
				p.logger.Printf("[%s] exited: %v (no restart)", p.conf.Name, err)
				p.setState(StateFailed)
			} else {
				p.logger.Printf("[%s] exited normally", p.conf.Name)
			}
			return
		}

		if err == nil {
			err = errors.New("exit status 0")
		}
		if time.Since(started) > stableAfter {
			backoff = minBackoff
		}
		p.logger.Printf("[%s] exited: %v; restarting in %s", p.conf.Name, err, backoff)
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
	p.logger.Printf("[%s] started pid=%d port=%d", p.conf.Name, cmd.Process.Pid, port)
	if out != nil {
		fmt.Fprintf(out, "\n=== yard: %s started pid=%d port=%d at %s ===\n",
			p.conf.Name, cmd.Process.Pid, port, time.Now().Format(time.RFC3339))
	}

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
			p.logger.Printf("[%s] port %d did not open within %s", p.conf.Name, port, p.conf.StartTimeout)
			p.terminate(tree, exited)
			return nil, fmt.Errorf("port %d did not open within %s (does the server listen on {port}?)", port, p.conf.StartTimeout)
		}
	}

	p.mu.Lock()
	p.setStateLocked(StateRunning)
	p.lastErr = ""
	p.mu.Unlock()
	p.logger.Printf("[%s] ready on port %d", p.conf.Name, port)

	select {
	case waitErr = <-exited:
		return exitStatus(cmd, waitErr)
	case <-ctx.Done():
		return p.terminate(tree, exited)
	}
}

// terminate stops gracefully, and kills if it does not exit in time.
func (p *Proc) terminate(tree *processTree, exited <-chan error) (*int, error) {
	p.logger.Printf("[%s] stopping", p.conf.Name)
	if err := tree.Interrupt(); err == nil {
		select {
		case <-exited:
			return nil, nil
		case <-time.After(stopTimeout):
		}
	}
	if err := tree.Kill(); err != nil {
		p.logger.Printf("[%s] kill: %v", p.conf.Name, err)
	}
	select {
	case <-exited:
	case <-time.After(stopTimeout):
		p.logger.Printf("[%s] did not exit after kill", p.conf.Name)
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
// If it opened an output file, that is returned too (the caller closes it).
func (p *Proc) command(port int) (*exec.Cmd, io.WriteCloser, error) {
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

	var out io.WriteCloser
	if t.Log != "" {
		f, err := OpenLog(t.Log)
		if err != nil {
			return nil, nil, fmt.Errorf("open log: %w", err)
		}
		cmd.Stdout, cmd.Stderr = f, f
		out = f
	}
	// If Stdout/Stderr are nil they are connected to os.DevNull (output is discarded).
	return cmd, out, nil
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

// OpenLog opens a log file for appending, creating the directory if needed.
func OpenLog(name string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
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
