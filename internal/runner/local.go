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
	"strings"
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
	opts        LocalOptions
	mu          sync.Mutex
	procs       map[string]*LocalProcess
	models      []ModelInfo
	modelsReady bool
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

func (l *Local) AvailableModels(ctx context.Context, refresh bool) ([]ModelInfo, error) {
	l.mu.Lock()
	if l.modelsReady && !refresh {
		models := cloneModels(l.models)
		l.mu.Unlock()
		return models, nil
	}
	l.mu.Unlock()

	models, err := l.queryAvailableModels(ctx)
	if err != nil {
		return nil, err
	}

	l.mu.Lock()
	l.models = cloneModels(models)
	l.modelsReady = true
	cached := cloneModels(l.models)
	l.mu.Unlock()
	return cached, nil
}

func (l *Local) ensure(ctx context.Context, req StartRequest) (*LocalProcess, bool, error) {
	l.mu.Lock()
	if proc := l.procs[req.SessionID]; proc != nil {
		l.mu.Unlock()
		return proc, false, nil
	}
	l.mu.Unlock()

	if _, err := l.AvailableModels(ctx, false); err != nil && l.opts.Logger != nil {
		l.opts.Logger.Warn("cache pi models failed", "error", err)
	}

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
	if req.TraceTelegramToken != "" && len(req.TraceTelegramChatIDs) > 0 {
		cmd.Env = append(cmd.Environ(),
			"PI_TRACE_TELEGRAM_BOT_TOKEN="+req.TraceTelegramToken,
			"PI_TRACE_TELEGRAM_CHAT_IDS="+int64ListString(req.TraceTelegramChatIDs),
		)
	}
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

func int64ListString(values []int64) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		if value == 0 {
			continue
		}
		parts = append(parts, strconv.FormatInt(value, 10))
	}
	return strings.Join(parts, ",")
}

func (l *Local) queryAvailableModels(ctx context.Context) ([]ModelInfo, error) {
	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(queryCtx, l.opts.Binary, "--mode", "rpc", "--no-session")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{"id": "models_1", "type": "get_available_models"})
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	if _, err := stdin.Write(append(payload, '\n')); err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	_ = stdin.Close()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var models []ModelInfo
	var responseErr error
	found := false
	for scanner.Scan() {
		var resp struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Command string `json:"command"`
			Success bool   `json:"success"`
			Error   string `json:"error"`
			Data    struct {
				Models []ModelInfo `json:"models"`
			} `json:"data"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			continue
		}
		if resp.ID != "models_1" || resp.Type != "response" || resp.Command != "get_available_models" {
			continue
		}
		found = true
		if !resp.Success {
			responseErr = errors.New(resp.Error)
			continue
		}
		models = resp.Data.Models
	}
	if err := scanner.Err(); err != nil && responseErr == nil {
		responseErr = err
	}
	if err := cmd.Wait(); err != nil && responseErr == nil && !found {
		responseErr = err
	}
	if responseErr != nil {
		return nil, responseErr
	}
	if queryCtx.Err() != nil {
		return nil, queryCtx.Err()
	}
	if len(models) == 0 {
		return nil, errors.New("pi returned no available models")
	}
	return models, nil
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

func cloneModels(models []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, len(models))
	for i, model := range models {
		out[i] = model
		out[i].Inputs = append([]string(nil), model.Inputs...)
	}
	return out
}

func (m *ModelInfo) UnmarshalJSON(data []byte) error {
	var raw struct {
		Provider      string   `json:"provider"`
		ID            string   `json:"id"`
		Name          string   `json:"name"`
		ContextWindow int64    `json:"contextWindow"`
		MaxTokens     int64    `json:"maxTokens"`
		Reasoning     bool     `json:"reasoning"`
		Input         []string `json:"input"`
		Inputs        []string `json:"inputs"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Provider = strings.TrimSpace(raw.Provider)
	m.ID = strings.TrimSpace(raw.ID)
	m.Name = strings.TrimSpace(raw.Name)
	m.ContextWindow = raw.ContextWindow
	m.MaxTokens = raw.MaxTokens
	m.Reasoning = raw.Reasoning
	m.Inputs = raw.Inputs
	if len(m.Inputs) == 0 {
		m.Inputs = raw.Input
	}
	return nil
}
