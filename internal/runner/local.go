package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

type LocalOptions struct {
	Binary      string
	SessionDir  string
	IdleTimeout time.Duration
	Logger      *slog.Logger
}

type Options = LocalOptions

type Local struct {
	opts  LocalOptions
	mu    sync.Mutex
	procs map[string]*LocalProcess
}

var _ Runner = (*Local)(nil)

type LocalProcess struct {
	sessionID string
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	mu        sync.Mutex
	streaming bool
	lastUsed  time.Time
}

func NewLocal(opts LocalOptions) *Local {
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = 5 * time.Minute
	}
	return &Local{opts: opts, procs: make(map[string]*LocalProcess)}
}

// NewManager is kept as a compatibility alias for older callers.
func NewManager(opts LocalOptions) *Local {
	return NewLocal(opts)
}

func (l *Local) Prompt(ctx context.Context, req StartRequest, message string) error {
	proc, _, err := l.ensure(ctx, req)
	if err != nil {
		return err
	}
	return proc.Send(map[string]any{"type": "prompt", "message": message})
}

func (l *Local) Steer(ctx context.Context, req StartRequest, message string) error {
	proc, fresh, err := l.ensure(ctx, req)
	if err != nil {
		return err
	}
	if fresh {
		return proc.Send(map[string]any{"type": "prompt", "message": message})
	}
	if proc.IsStreaming() {
		return proc.Send(map[string]any{"type": "steer", "message": message})
	}
	return proc.Send(map[string]any{"type": "prompt", "message": message})
}

func (l *Local) ensure(ctx context.Context, req StartRequest) (*LocalProcess, bool, error) {
	l.mu.Lock()
	if proc := l.procs[req.SessionID]; proc != nil {
		l.mu.Unlock()
		return proc, false, nil
	}
	l.mu.Unlock()

	args := []string{"--mode", "rpc", "--session-dir", l.opts.SessionDir, "--topic", intString(req.TopicID)}
	if req.SessionID != "" {
		args = append(args, "--session-id", req.SessionID)
	}
	if req.Title != "" {
		args = append(args, "--name", req.Title)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	cmd := exec.CommandContext(ctx, l.opts.Binary, args...)
	cmd.Dir = req.Workspace
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, false, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}
	proc := &LocalProcess{sessionID: req.SessionID, cmd: cmd, stdin: stdin, streaming: true, lastUsed: time.Now()}
	l.mu.Lock()
	l.procs[req.SessionID] = proc
	l.mu.Unlock()
	go l.watch(proc, stdout)
	return proc, true, nil
}

func (l *Local) watch(proc *LocalProcess, r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		var event struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(line, &event)
		if event.Type == "agent_end" || event.Type == "done" {
			proc.setStreaming(false)
			go l.idleKill(proc)
		}
		if l.opts.Logger != nil {
			l.opts.Logger.Debug("pi output", "session", proc.sessionID, "line", string(line))
		}
	}
	_ = proc.cmd.Wait()
	l.mu.Lock()
	delete(l.procs, proc.sessionID)
	l.mu.Unlock()
}

func (l *Local) idleKill(proc *LocalProcess) {
	time.Sleep(l.opts.IdleTimeout)
	proc.mu.Lock()
	defer proc.mu.Unlock()
	if proc.streaming || time.Since(proc.lastUsed) < l.opts.IdleTimeout {
		return
	}
	if proc.cmd.Process != nil {
		_ = proc.cmd.Process.Kill()
	}
}

func (p *LocalProcess) Send(payload map[string]any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdin == nil {
		return errors.New("process stdin is closed")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := p.stdin.Write(append(data, '\n')); err != nil {
		return err
	}
	p.streaming = true
	p.lastUsed = time.Now()
	return nil
}

func (p *LocalProcess) IsStreaming() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.streaming
}

func (p *LocalProcess) setStreaming(v bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.streaming = v
	p.lastUsed = time.Now()
}

func intString(v int) string {
	return strconv.Itoa(v)
}
