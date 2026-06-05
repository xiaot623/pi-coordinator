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

type Options struct {
	Binary      string
	SessionDir  string
	BotToken    string
	GroupChatID int64
	IdleTimeout time.Duration
	Logger      *slog.Logger
}

type Manager struct {
	opts  Options
	mu    sync.Mutex
	procs map[string]*Process
}

type Process struct {
	sessionID string
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	mu        sync.Mutex
	streaming bool
	lastUsed  time.Time
}

func NewManager(opts Options) *Manager {
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = 5 * time.Minute
	}
	return &Manager{opts: opts, procs: make(map[string]*Process)}
}

func (m *Manager) Prompt(ctx context.Context, req StartRequest, message string) error {
	proc, fresh, err := m.ensure(ctx, req)
	if err != nil {
		return err
	}
	if fresh && req.SessionID != "" && req.Existing {
		if err := proc.Send(map[string]any{"type": "switch_session", "session_id": req.SessionID}); err != nil {
			return err
		}
	} else if fresh && !req.Existing {
		if err := proc.Send(map[string]any{"type": "new_session"}); err != nil {
			return err
		}
	}
	return proc.Send(map[string]any{"type": "prompt", "message": message})
}

func (m *Manager) Steer(ctx context.Context, req StartRequest, message string) error {
	proc, fresh, err := m.ensure(ctx, req)
	if err != nil {
		return err
	}
	if fresh && req.SessionID != "" {
		if err := proc.Send(map[string]any{"type": "switch_session", "session_id": req.SessionID}); err != nil {
			return err
		}
		return proc.Send(map[string]any{"type": "prompt", "message": message})
	}
	if proc.IsStreaming() {
		return proc.Send(map[string]any{"type": "steer", "message": message})
	}
	return proc.Send(map[string]any{"type": "prompt", "message": message})
}

type StartRequest struct {
	SessionID string
	Title     string
	Workspace string
	TopicID   int
	Model     string
	Existing  bool
}

func (m *Manager) ensure(ctx context.Context, req StartRequest) (*Process, bool, error) {
	m.mu.Lock()
	if proc := m.procs[req.SessionID]; proc != nil {
		m.mu.Unlock()
		return proc, false, nil
	}
	m.mu.Unlock()

	args := []string{"--mode", "rpc", "--session-dir", m.opts.SessionDir, "--topic", intString(req.TopicID)}
	if req.Title != "" {
		args = append(args, "--name", req.Title)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	cmd := exec.CommandContext(ctx, m.opts.Binary, args...)
	cmd.Dir = req.Workspace
	cmd.Env = append(cmd.Environ(),
		"PI_TRACE_TELEGRAM_BOT_TOKEN="+m.opts.BotToken,
		"PI_TRACE_TELEGRAM_CHAT_IDS="+int64String(m.opts.GroupChatID),
	)
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
	proc := &Process{sessionID: req.SessionID, cmd: cmd, stdin: stdin, streaming: true, lastUsed: time.Now()}
	m.mu.Lock()
	m.procs[req.SessionID] = proc
	m.mu.Unlock()
	go m.watch(proc, stdout)
	return proc, true, nil
}

func (m *Manager) watch(proc *Process, r io.Reader) {
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
			go m.idleKill(proc)
		}
		if m.opts.Logger != nil {
			m.opts.Logger.Debug("pi output", "session", proc.sessionID, "line", string(line))
		}
	}
	_ = proc.cmd.Wait()
	m.mu.Lock()
	delete(m.procs, proc.sessionID)
	m.mu.Unlock()
}

func (m *Manager) idleKill(proc *Process) {
	time.Sleep(m.opts.IdleTimeout)
	proc.mu.Lock()
	defer proc.mu.Unlock()
	if proc.streaming || time.Since(proc.lastUsed) < m.opts.IdleTimeout {
		return
	}
	if proc.cmd.Process != nil {
		_ = proc.cmd.Process.Kill()
	}
}

func (p *Process) Send(payload map[string]any) error {
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

func (p *Process) IsStreaming() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.streaming
}

func (p *Process) setStreaming(v bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.streaming = v
	p.lastUsed = time.Now()
}

func intString(v int) string {
	return strconv.Itoa(v)
}

func int64String(v int64) string {
	return strconv.FormatInt(v, 10)
}
