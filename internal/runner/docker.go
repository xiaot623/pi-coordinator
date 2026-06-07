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

type DockerOptions struct {
	Binary               string
	Image                string
	ContainerHome        string
	AgentMountMode       string
	HostAgentDir         string
	HostPluginDir        string
	HostSkillsDir        string
	HostSessionDir       string
	IdleTimeout          time.Duration
	Plugins              []string
	PluginUpdateInterval time.Duration
	Logger               *slog.Logger
}

type Docker struct {
	opts        DockerOptions
	mu          sync.Mutex
	procs       map[string]*LocalProcess
	models      []ModelInfo
	modelsReady bool
}

var _ Runner = (*Docker)(nil)

func NewDocker(opts DockerOptions) *Docker {
	if opts.Binary == "" {
		opts.Binary = "pi"
	}
	if opts.Image == "" {
		opts.Image = "pi-agent:latest"
	}
	if opts.ContainerHome == "" {
		opts.ContainerHome = "/home/pi"
	}
	if opts.AgentMountMode != "ro" {
		opts.AgentMountMode = "rw"
	}
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = 5 * time.Minute
	}
	return &Docker{opts: opts, procs: make(map[string]*LocalProcess)}
}

func (d *Docker) Prompt(ctx context.Context, req StartRequest, message string, images []ImageAttachment) error {
	proc, _, err := d.ensure(ctx, req)
	if err != nil {
		return err
	}
	return proc.Send(buildRPCPayload("prompt", message, images))
}

func (d *Docker) Steer(ctx context.Context, req StartRequest, message string, images []ImageAttachment) error {
	proc, fresh, err := d.ensure(ctx, req)
	if err != nil {
		return err
	}
	if fresh {
		return proc.Send(buildRPCPayload("prompt", message, images))
	}
	if proc.IsStreaming() {
		return proc.Send(buildRPCPayload("steer", message, images))
	}
	return proc.Send(buildRPCPayload("prompt", message, images))
}

func (d *Docker) AvailableModels(ctx context.Context, refresh bool) ([]ModelInfo, error) {
	d.mu.Lock()
	if d.modelsReady && !refresh {
		models := cloneModels(d.models)
		d.mu.Unlock()
		return models, nil
	}
	d.mu.Unlock()
	models, err := d.queryAvailableModels(ctx)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.models = cloneModels(models)
	d.modelsReady = true
	cached := cloneModels(d.models)
	d.mu.Unlock()
	return cached, nil
}

func (d *Docker) ensure(ctx context.Context, req StartRequest) (*LocalProcess, bool, error) {
	d.mu.Lock()
	if proc := d.procs[req.SessionID]; proc != nil {
		d.mu.Unlock()
		return proc, false, nil
	}
	d.mu.Unlock()

	if _, err := d.AvailableModels(ctx, false); err != nil && d.opts.Logger != nil {
		d.opts.Logger.Warn("cache pi models in docker failed", "error", err)
	}

	args, err := d.containerArgs(ctx, req)
	if err != nil {
		return nil, false, err
	}
	if d.opts.Logger != nil {
		d.opts.Logger.Info("starting docker pi", "session", req.SessionID, "image", d.opts.Image, "workspace", req.Workspace)
		d.opts.Logger.Debug("docker pi args", "session", req.SessionID, "args", sanitizeDockerArgs(args))
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
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
	d.mu.Lock()
	d.procs[req.SessionID] = proc
	d.mu.Unlock()
	go d.watch(proc, stdout)
	return proc, true, nil
}

func (d *Docker) containerArgs(ctx context.Context, req StartRequest) ([]string, error) {
	if req.Workspace == "" {
		return nil, errors.New("docker runner requires a worktree workspace")
	}
	if err := os.MkdirAll(d.opts.HostSessionDir, 0o755); err != nil {
		return nil, err
	}
	if err := requireDir(d.opts.HostAgentDir); err != nil {
		return nil, err
	}
	if err := requireDir(d.opts.HostPluginDir); err != nil {
		return nil, err
	}
	if d.opts.HostSkillsDir != "" {
		if err := os.MkdirAll(d.opts.HostSkillsDir, 0o755); err != nil {
			return nil, err
		}
	}
	gitMounts, err := d.gitMounts(ctx, req.Workspace)
	if err != nil {
		return nil, err
	}
	piArgs, err := d.piArgs(ctx, req, true)
	if err != nil {
		return nil, err
	}
	args := []string{
		"run", "--rm",
		"--user", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()),
		"-e", "HOME=" + d.opts.ContainerHome,
		"-e", "NPM_CONFIG_CACHE=/tmp/npm-cache",
		"-v", req.Workspace + ":" + req.Workspace + ":rw",
	}
	if req.TraceTelegramToken != "" && len(req.TraceTelegramChatIDs) > 0 {
		args = append(args,
			"-e", "PI_TRACE_TELEGRAM_BOT_TOKEN="+req.TraceTelegramToken,
			"-e", "PI_TRACE_TELEGRAM_CHAT_IDS="+int64ListString(req.TraceTelegramChatIDs),
		)
	}
	for _, mount := range gitMounts {
		args = append(args, "-v", mount+":"+mount+":rw")
	}
	args = append(args,
		"-v", d.opts.HostAgentDir+":"+filepath.Join(d.opts.ContainerHome, ".pi", "agent")+":"+d.opts.AgentMountMode,
		"-v", d.opts.HostPluginDir+":"+filepath.Join(d.opts.ContainerHome, ".pi", "pico", "agent")+":ro",
		"-v", d.opts.HostSessionDir+":"+filepath.Join(d.opts.ContainerHome, ".pi", "pico", "sessions", "docker")+":rw",
	)
	if d.opts.HostSkillsDir != "" {
		args = append(args, "-v", d.opts.HostSkillsDir+":"+filepath.Join(d.opts.ContainerHome, ".agents", "skills")+":ro")
	}
	args = append(args,
		"-w", req.Workspace,
		"-i",
		d.opts.Image,
	)
	args = append(args, piArgs...)
	return args, nil
}

func (d *Docker) queryAvailableModels(ctx context.Context) ([]ModelInfo, error) {
	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := requireDir(d.opts.HostAgentDir); err != nil {
		return nil, err
	}
	args := []string{
		"run", "--rm",
		"-e", "HOME=" + d.opts.ContainerHome,
		"-e", "NPM_CONFIG_CACHE=/tmp/npm-cache",
		"-v", d.opts.HostAgentDir + ":" + filepath.Join(d.opts.ContainerHome, ".pi", "agent") + ":" + d.opts.AgentMountMode,
		"-i",
		d.opts.Image,
		d.opts.Binary, "--mode", "rpc", "--no-session",
	}
	cmd := exec.CommandContext(queryCtx, "docker", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if d.opts.Logger != nil {
		d.opts.Logger.Debug("query docker pi models", "image", d.opts.Image, "args", sanitizeDockerArgs(args))
	}
	cmd.Stderr = cmd.Stdout
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
	return readModelsResponse(queryCtx, cmd, stdout)
}

func (d *Docker) piArgs(ctx context.Context, req StartRequest, convertPlugins bool) ([]string, error) {
	sessionDir := filepath.Join(d.opts.ContainerHome, ".pi", "pico", "sessions", "docker")
	args := []string{d.opts.Binary}
	pluginArgs, err := d.pluginArgs(ctx, convertPlugins)
	if err != nil {
		return nil, err
	}
	if len(pluginArgs) > 0 {
		args = append(args, pluginArgs...)
	}
	args = append(args, "--mode", "rpc", "--session-dir", sessionDir)
	if len(d.opts.Plugins) > 0 {
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
	return args, nil
}

func (d *Docker) pluginArgs(ctx context.Context, convert bool) ([]string, error) {
	helper := NewLocal(LocalOptions{
		Binary:               d.opts.Binary,
		SessionDir:           d.opts.HostSessionDir,
		Plugins:              d.opts.Plugins,
		PluginAgentDir:       d.opts.HostPluginDir,
		PluginUpdateInterval: d.opts.PluginUpdateInterval,
		Logger:               d.opts.Logger,
	})
	args, err := helper.resolvedPluginArgs(ctx)
	if err != nil || !convert {
		return args, err
	}
	hostRoot, err := filepath.Abs(d.opts.HostPluginDir)
	if err != nil {
		return nil, err
	}
	containerRoot := filepath.Join(d.opts.ContainerHome, ".pi", "pico", "agent")
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "--extension" {
			continue
		}
		hostPath, err := filepath.Abs(args[i+1])
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(hostPath); err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(hostRoot, hostPath)
		if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			return nil, fmt.Errorf("docker plugin %s is outside %s", hostPath, hostRoot)
		}
		args[i+1] = filepath.Join(containerRoot, rel)
	}
	return args, nil
}

func (d *Docker) gitMounts(ctx context.Context, worktree string) ([]string, error) {
	gitDir, err := gitOutput(ctx, worktree, "rev-parse", "--git-dir")
	if err != nil {
		return nil, err
	}
	commonDir, err := gitOutput(ctx, worktree, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	return compactMounts(absGitPath(worktree, gitDir), absGitPath(worktree, commonDir)), nil
}

func absGitPath(worktree, raw string) string {
	path := strings.TrimSpace(raw)
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(worktree, path))
}

func compactMounts(paths ...string) []string {
	var cleaned []string
	for _, path := range paths {
		path = filepath.Clean(path)
		dup := false
		for _, existing := range cleaned {
			if path == existing || strings.HasPrefix(path, existing+string(filepath.Separator)) {
				dup = true
				break
			}
		}
		if !dup {
			cleaned = append(cleaned, path)
		}
	}
	return cleaned
}

func requireDir(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	return nil
}

func readModelsResponse(ctx context.Context, cmd *exec.Cmd, stdout io.Reader) ([]ModelInfo, error) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var models []ModelInfo
	var responseErr error
	var outputSample []string
	found := false
	for scanner.Scan() {
		line := string(scanner.Bytes())
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
			outputSample = appendSample(outputSample, line)
			continue
		}
		if resp.ID != "models_1" || resp.Type != "response" || resp.Command != "get_available_models" {
			outputSample = appendSample(outputSample, line)
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
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if len(models) == 0 {
		if len(outputSample) > 0 {
			return nil, fmt.Errorf("pi returned no available models; output: %s", strings.Join(outputSample, "\n"))
		}
		return nil, errors.New("pi returned no available models")
	}
	return models, nil
}

func (d *Docker) watch(proc *LocalProcess, r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		var event struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			if d.opts.Logger != nil {
				d.opts.Logger.Warn("docker pi non-json output", "session", proc.sessionID, "line", string(line))
			}
			continue
		}
		if event.Type == "agent_end" || event.Type == "done" {
			proc.setStreaming(false)
			go d.idleKill(proc)
			if d.opts.Logger != nil {
				d.opts.Logger.Info("docker pi completed event", "session", proc.sessionID, "event", event.Type)
			}
		}
		if d.opts.Logger != nil {
			d.opts.Logger.Debug("docker pi output", "session", proc.sessionID, "line", string(line))
		}
	}
	if err := scanner.Err(); err != nil && d.opts.Logger != nil {
		d.opts.Logger.Warn("docker pi output scanner failed", "session", proc.sessionID, "error", err)
	}
	err := proc.cmd.Wait()
	if d.opts.Logger != nil {
		if err != nil {
			d.opts.Logger.Warn("docker pi exited with error", "session", proc.sessionID, "error", err)
		} else {
			d.opts.Logger.Info("docker pi exited", "session", proc.sessionID)
		}
	}
	d.mu.Lock()
	delete(d.procs, proc.sessionID)
	d.mu.Unlock()
}

func appendSample(sample []string, line string) []string {
	if len(sample) >= 5 {
		return sample
	}
	line = strings.TrimSpace(line)
	if len(line) > 500 {
		line = line[:500] + "..."
	}
	if line == "" {
		return sample
	}
	return append(sample, line)
}

func sanitizeDockerArgs(args []string) []string {
	out := append([]string(nil), args...)
	for i, arg := range out {
		if strings.HasPrefix(arg, "PI_TRACE_TELEGRAM_BOT_TOKEN=") {
			out[i] = "PI_TRACE_TELEGRAM_BOT_TOKEN=<redacted>"
		}
	}
	return out
}

func (d *Docker) idleKill(proc *LocalProcess) {
	time.Sleep(d.opts.IdleTimeout)
	proc.mu.Lock()
	defer proc.mu.Unlock()
	if proc.streaming || time.Since(proc.lastUsed) < d.opts.IdleTimeout {
		return
	}
	if proc.cmd.Process != nil {
		_ = proc.cmd.Process.Kill()
	}
}
