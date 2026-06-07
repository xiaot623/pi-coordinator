package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type LocalOptions struct {
	Binary               string
	SessionDir           string
	IdleTimeout          time.Duration
	Plugins              []string
	PluginAgentDir       string
	PluginUpdateInterval time.Duration
	Logger               *slog.Logger
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
	sessionID    string
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	mu           sync.Mutex
	streaming    bool
	startedAt    time.Time
	lastUsed     time.Time
	currentModel string
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

func (l *Local) Prompt(ctx context.Context, req StartRequest, message string, images []ImageAttachment) error {
	proc, _, err := l.ensure(ctx, req)
	if err != nil {
		return err
	}
	return proc.SendWithModel(req.Model, buildRPCPayload("prompt", message, images))
}

func (l *Local) Steer(ctx context.Context, req StartRequest, message string, images []ImageAttachment) error {
	proc, fresh, err := l.ensure(ctx, req)
	if err != nil {
		return err
	}
	if fresh {
		return proc.SendWithModel(req.Model, buildRPCPayload("prompt", message, images))
	}
	if proc.IsStreaming() {
		return proc.SendWithModel(req.Model, buildRPCPayload("steer", message, images))
	}
	return proc.SendWithModel(req.Model, buildRPCPayload("prompt", message, images))
}

func buildRPCPayload(kind string, message string, images []ImageAttachment) map[string]any {
	payload := map[string]any{"type": kind, "message": message}
	if len(images) > 0 {
		payload["images"] = images
	}
	return payload
}

func buildSetModelPayload(model string) (map[string]any, error) {
	provider, modelID, err := splitModelRef(model)
	if err != nil {
		return nil, err
	}
	return map[string]any{"type": "set_model", "provider": provider, "modelId": modelID}, nil
}

func splitModelRef(model string) (string, string, error) {
	model = strings.TrimSpace(model)
	provider, modelID, ok := strings.Cut(model, "/")
	provider = strings.TrimSpace(provider)
	modelID = strings.TrimSpace(modelID)
	if !ok || provider == "" || modelID == "" {
		return "", "", fmt.Errorf("invalid model %q; expected provider/model", model)
	}
	return provider, modelID, nil
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

func (l *Local) ActiveProcesses() []ProcessInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]ProcessInfo, 0, len(l.procs))
	for sessionID, proc := range l.procs {
		proc.mu.Lock()
		pid := 0
		if proc.cmd != nil && proc.cmd.Process != nil {
			pid = proc.cmd.Process.Pid
		}
		out = append(out, ProcessInfo{
			SessionID: sessionID,
			PID:       pid,
			Busy:      proc.streaming,
			StartedAt: proc.startedAt,
			LastUsed:  proc.lastUsed,
		})
		proc.mu.Unlock()
	}
	return out
}

func (l *Local) StopSession(ctx context.Context, sessionID string) error {
	_ = ctx
	l.mu.Lock()
	proc := l.procs[sessionID]
	l.mu.Unlock()
	if proc == nil {
		return ErrSessionNotActive
	}
	proc.mu.Lock()
	defer proc.mu.Unlock()
	if proc.stdin != nil {
		_ = proc.stdin.Close()
		proc.stdin = nil
	}
	if proc.cmd == nil || proc.cmd.Process == nil {
		return ErrSessionNotActive
	}
	if err := proc.cmd.Process.Kill(); err != nil {
		return err
	}
	l.mu.Lock()
	delete(l.procs, sessionID)
	l.mu.Unlock()
	return nil
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
	if l.opts.SessionDir != "" {
		if err := os.MkdirAll(l.opts.SessionDir, 0o755); err != nil {
			return nil, false, err
		}
	}

	args, err := l.baseArgs(ctx, "--mode", "rpc", "--session-dir", l.opts.SessionDir)
	if err != nil {
		return nil, false, err
	}
	if len(l.opts.Plugins) > 0 {
		args = append(args, "--topic", intString(req.TopicID))
	}
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
	now := time.Now()
	proc := &LocalProcess{sessionID: req.SessionID, cmd: cmd, stdin: stdin, streaming: true, startedAt: now, lastUsed: now, currentModel: req.Model}
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
	args, err := l.baseArgs(ctx, "--mode", "rpc", "--no-session")
	if err != nil {
		return nil, err
	}

	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(queryCtx, l.opts.Binary, args...)
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

func (l *Local) baseArgs(ctx context.Context, args ...string) ([]string, error) {
	out := append([]string(nil), args...)
	pluginArgs, err := l.resolvedPluginArgs(ctx)
	if err != nil {
		return nil, err
	}
	if len(pluginArgs) > 0 {
		withPlugins := make([]string, 0, len(out)+len(pluginArgs))
		withPlugins = append(withPlugins, pluginArgs...)
		withPlugins = append(withPlugins, out...)
		return withPlugins, nil
	}
	return out, nil
}

func (l *Local) resolvedPluginArgs(ctx context.Context) ([]string, error) {
	paths, err := l.resolvePlugins(ctx)
	if err != nil {
		return nil, err
	}
	args := []string{"--no-extensions"}
	for _, path := range paths {
		args = append(args, "--extension", path)
	}
	return args, nil
}

func (l *Local) resolvePlugins(ctx context.Context) ([]string, error) {
	paths := make([]string, 0, len(l.opts.Plugins))
	for _, plugin := range l.opts.Plugins {
		plugin = strings.TrimSpace(plugin)
		if plugin == "" {
			continue
		}
		resolved, err := l.resolvePlugin(ctx, plugin)
		if err != nil {
			return nil, err
		}
		paths = append(paths, resolved...)
	}
	return paths, nil
}

func (l *Local) resolvePlugin(ctx context.Context, plugin string) ([]string, error) {
	if isLocalPluginPath(plugin) {
		if _, err := os.Stat(plugin); err != nil {
			return nil, fmt.Errorf("plugin %q not found: %w", plugin, err)
		}
		return []string{plugin}, nil
	}
	if strings.HasPrefix(plugin, "npm:") || !strings.Contains(plugin, ":") {
		return l.resolveNpmPlugin(ctx, plugin)
	}
	return nil, fmt.Errorf("unsupported plugin %q; use an npm package name or local extension path", plugin)
}

func (l *Local) resolveNpmPlugin(ctx context.Context, source string) ([]string, error) {
	spec := strings.TrimPrefix(source, "npm:")
	name := npmPackageName(spec)
	if name == "" {
		return nil, fmt.Errorf("invalid npm plugin %q", source)
	}
	if l.opts.PluginAgentDir == "" {
		return nil, errors.New("plugin agent dir is required for npm plugins")
	}

	packageDir := filepath.Join(l.opts.PluginAgentDir, "npm", "node_modules", filepath.FromSlash(name))
	source = "npm:" + spec
	if _, err := os.Stat(filepath.Join(packageDir, "package.json")); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err := l.installNpmPlugin(ctx, spec); err != nil {
			return nil, err
		}
		if err := l.writePluginUpdatedAt(source, time.Now()); err != nil && l.opts.Logger != nil {
			l.opts.Logger.Warn("record pi plugin update time failed", "source", source, "error", err)
		}
	} else if !l.pluginSourceInSettings(source) {
		if err := l.installNpmPlugin(ctx, spec); err != nil {
			return nil, err
		}
		if err := l.writePluginUpdatedAt(source, time.Now()); err != nil && l.opts.Logger != nil {
			l.opts.Logger.Warn("record pi plugin update time failed", "source", source, "error", err)
		}
	} else if l.pluginUpdateDue(source, time.Now()) {
		if err := l.updateNpmPlugin(ctx, source); err != nil {
			return nil, err
		}
		if err := l.writePluginUpdatedAt(source, time.Now()); err != nil && l.opts.Logger != nil {
			l.opts.Logger.Warn("record pi plugin update time failed", "source", source, "error", err)
		}
	}
	return extensionEntries(packageDir)
}

func (l *Local) installNpmPlugin(ctx context.Context, spec string) error {
	source := "npm:" + spec
	if l.opts.Logger != nil {
		l.opts.Logger.Info("install pi plugin", "source", source, "agent_dir", l.opts.PluginAgentDir)
	}
	cmd := exec.CommandContext(ctx, l.opts.Binary, "install", source)
	cmd.Env = append(cmd.Environ(), "PI_CODING_AGENT_DIR="+l.opts.PluginAgentDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("install plugin %s: %w: %s", source, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (l *Local) updateNpmPlugin(ctx context.Context, source string) error {
	if l.opts.Logger != nil {
		l.opts.Logger.Info("update pi plugin", "source", source, "agent_dir", l.opts.PluginAgentDir)
	}
	cmd := exec.CommandContext(ctx, l.opts.Binary, "update", source)
	cmd.Env = append(cmd.Environ(), "PI_CODING_AGENT_DIR="+l.opts.PluginAgentDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("update plugin %s: %w: %s", source, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (l *Local) pluginSourceInSettings(source string) bool {
	data, err := os.ReadFile(filepath.Join(l.opts.PluginAgentDir, "settings.json"))
	if err != nil {
		return false
	}
	var settings struct {
		Packages []any `json:"packages"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return false
	}
	for _, raw := range settings.Packages {
		switch pkg := raw.(type) {
		case string:
			if pkg == source {
				return true
			}
		case map[string]any:
			if pkg["source"] == source {
				return true
			}
		}
	}
	return false
}

func (l *Local) pluginUpdateDue(source string, now time.Time) bool {
	if l.opts.PluginUpdateInterval < 0 {
		return false
	}
	if l.opts.PluginUpdateInterval == 0 {
		return true
	}
	updatedAt, ok := l.pluginUpdatedAt(source)
	if !ok {
		return true
	}
	return !updatedAt.After(now) && now.Sub(updatedAt) >= l.opts.PluginUpdateInterval
}

func (l *Local) pluginUpdatedAt(source string) (time.Time, bool) {
	updates, err := l.readPluginUpdateTimes()
	if err != nil {
		return time.Time{}, false
	}
	raw := strings.TrimSpace(updates[source])
	if raw == "" {
		return time.Time{}, false
	}
	updatedAt, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return updatedAt, true
}

func (l *Local) writePluginUpdatedAt(source string, updatedAt time.Time) error {
	if l.opts.PluginAgentDir == "" {
		return errors.New("plugin agent dir is required")
	}
	updates, err := l.readPluginUpdateTimes()
	if err != nil {
		if l.opts.Logger != nil {
			l.opts.Logger.Warn("reset pi plugin update times", "error", err)
		}
		updates = map[string]string{}
	}
	updates[source] = updatedAt.UTC().Format(time.RFC3339)
	if err := os.MkdirAll(l.opts.PluginAgentDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(updates, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(l.pluginUpdatesPath(), data, 0o600)
}

func (l *Local) readPluginUpdateTimes() (map[string]string, error) {
	updates := map[string]string{}
	data, err := os.ReadFile(l.pluginUpdatesPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return updates, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return updates, nil
	}
	if err := json.Unmarshal(data, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

func (l *Local) pluginUpdatesPath() string {
	return filepath.Join(l.opts.PluginAgentDir, "plugin-updates.json")
}

func extensionEntries(packageDir string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(packageDir, "package.json"))
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Pi struct {
			Extensions []string `json:"extensions"`
		} `json:"pi"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	candidates := manifest.Pi.Extensions
	if len(candidates) == 0 {
		candidates = []string{"index.ts", "index.js"}
	}
	paths := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		path := candidate
		if !filepath.IsAbs(path) {
			path = filepath.Join(packageDir, filepath.FromSlash(candidate))
		}
		if _, err := os.Stat(path); err != nil {
			if len(manifest.Pi.Extensions) > 0 {
				return nil, fmt.Errorf("extension entry %q not found in %s: %w", candidate, packageDir, err)
			}
			continue
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("plugin package %s does not declare pi.extensions and has no index.ts/index.js", packageDir)
	}
	return paths, nil
}

func isLocalPluginPath(plugin string) bool {
	return filepath.IsAbs(plugin) || strings.HasPrefix(plugin, ".") || strings.HasPrefix(plugin, "~")
}

func npmPackageName(spec string) string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return ""
	}
	if strings.HasPrefix(spec, "@") {
		slash := strings.Index(spec, "/")
		if slash < 0 {
			return ""
		}
		if version := strings.Index(spec[slash+1:], "@"); version >= 0 {
			return spec[:slash+1+version]
		}
		return spec
	}
	if version := strings.Index(spec, "@"); version >= 0 {
		return spec[:version]
	}
	return spec
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
	if err := p.writePayloadLocked(payload); err != nil {
		return err
	}
	p.streaming = true
	p.lastUsed = time.Now()
	return nil
}

func (p *LocalProcess) SendWithModel(model string, payload map[string]any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if strings.TrimSpace(model) != "" && model != p.currentModel {
		setModelPayload, err := buildSetModelPayload(model)
		if err != nil {
			return err
		}
		if err := p.writePayloadLocked(setModelPayload); err != nil {
			return err
		}
		p.currentModel = model
		p.lastUsed = time.Now()
	}
	if err := p.writePayloadLocked(payload); err != nil {
		return err
	}
	p.streaming = true
	p.lastUsed = time.Now()
	return nil
}

func (p *LocalProcess) writePayloadLocked(payload map[string]any) error {
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
