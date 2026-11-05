package upstream

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Command says how to start a stdio upstream server.
type Command struct {
	Path string
	Args []string
}

// Process is an upstream MCP server running as a child process, talking over
// its stdin and stdout. Call and Notify come from the embedded Conn.
type Process struct {
	*Conn
	cmd             *exec.Cmd
	stdin           io.WriteCloser
	shutdownTimeout time.Duration

	closeOnce sync.Once
	closeErr  error
}

// Start launches the command and connects to it. The child inherits the
// gateway's environment.
func Start(command Command, opts Options) (*Process, error) {
	opts = opts.withDefaults()
	cmd := exec.Command(command.Path, command.Args...)
	cmd.Stderr = opts.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("upstream: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("upstream: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("upstream: start %s: %w", command.Path, err)
	}

	return &Process{
		Conn:            NewConn(stdout, stdin, opts),
		cmd:             cmd,
		stdin:           stdin,
		shutdownTimeout: opts.ShutdownTimeout,
	}, nil
}

// Close shuts the process down the way the MCP stdio spec describes: close
// its stdin, wait for it to exit, and escalate to SIGTERM and then SIGKILL if
// it does not. It returns nil only for a clean, prompt exit with status 0.
// Calling Close again returns the same result.
func (p *Process) Close() error {
	p.closeOnce.Do(func() { p.closeErr = p.shutdown() })
	return p.closeErr
}

func (p *Process) shutdown() error {
	var errs []error
	if err := p.stdin.Close(); err != nil {
		errs = append(errs, fmt.Errorf("upstream: close stdin: %w", err))
	}

	// The read loop ends when the child closes stdout, which normally means it
	// exited. Wait must not be called before that, because it closes stdout.
	if !waitFor(p.Done(), p.shutdownTimeout) {
		errs = append(errs, errors.New("upstream: process did not exit after stdin was closed"))
		if err := p.terminate(); err != nil {
			errs = append(errs, err)
		}
	}

	if err := p.wait(); err != nil {
		errs = append(errs, fmt.Errorf("upstream: process exited: %w", err))
	}
	// Wait closed stdout if it was still open, so the read loop is ending.
	<-p.Done()
	return errors.Join(errs...)
}

// terminate sends SIGTERM, then SIGKILL if the process is still running
// after the shutdown timeout.
func (p *Process) terminate() error {
	// Signal fails on platforms without SIGTERM (Windows); go straight to kill.
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err == nil && waitFor(p.Done(), p.shutdownTimeout) {
		return nil
	}
	return p.kill()
}

func (p *Process) kill() error {
	if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("upstream: kill process: %w", err)
	}
	return nil
}

// wait reaps the process. If stdout closed but the process keeps running,
// it is killed after the shutdown timeout so Close cannot hang.
func (p *Process) wait() error {
	result := make(chan error, 1)
	go func() { result <- p.cmd.Wait() }()

	select {
	case err := <-result:
		return err
	case <-time.After(p.shutdownTimeout):
		if err := p.kill(); err != nil {
			return errors.Join(err, <-result)
		}
		return <-result
	}
}

func waitFor(ch <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	}
}
