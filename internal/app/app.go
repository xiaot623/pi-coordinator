package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/xiaot/pi-coordinator/internal/config"
	"github.com/xiaot/pi-coordinator/internal/runner"
	"github.com/xiaot/pi-coordinator/internal/session"
	"github.com/xiaot/pi-coordinator/internal/store"
)

// App is the core coordinator for pi.
// It manages workspaces, sessions, and coordinates with the pi runner.
type App struct {
	cfg    *config.Config // Kept as a pointer; note: config hot-reload may require atomic/mutex if we want dynamic reloading in App
	paths  config.Paths
	log    *slog.Logger
	store  *store.Store
	local  runner.Runner
	work   runner.Runner
	docker runner.Runner
	wt     *runner.WorktreeManager
}

func New(cfg config.Config, paths config.Paths, logger *slog.Logger) (*App, error) {
	st, err := store.Open(paths.DBPath)
	if err != nil {
		return nil, err
	}
	pluginDir := filepath.Join(paths.DataDir, "agent")
	localOpts := runner.LocalOptions{
		Binary:               cfg.Runner.Binary,
		SessionDir:           cfg.Runner.SessionDir,
		IdleTimeout:          cfg.Runner.IdleTimeout.Duration,
		Plugins:              cfg.Runner.Plugins,
		PluginAgentDir:       pluginDir,
		PluginUpdateInterval: time.Duration(cfg.Runner.PluginUpdateIntervalMinutes) * time.Minute,
		Logger:               logger,
	}
	rm := runner.NewLocal(localOpts)
	workSessionDir := filepath.Join(paths.DataDir, "sessions", "worktree")
	dockerSessionDir := filepath.Join(paths.DataDir, "sessions", "docker")
	work := runner.NewWorktreeRunner(runner.WorktreeOptions{LocalOptions: localOpts, SessionDir: workSessionDir, Logger: logger})
	home, _ := os.UserHomeDir()
	docker := runner.NewDocker(runner.DockerOptions{
		Binary:               cfg.Runner.Binary,
		Image:                cfg.Runner.DockerImage,
		HostAgentDir:         filepath.Join(home, ".pi", "agent"),
		HostPluginDir:        pluginDir,
		HostSkillsDir:        filepath.Join(home, ".agents", "skills"),
		HostSessionDir:       dockerSessionDir,
		IdleTimeout:          cfg.Runner.IdleTimeout.Duration,
		Plugins:              cfg.Runner.Plugins,
		PluginUpdateInterval: time.Duration(cfg.Runner.PluginUpdateIntervalMinutes) * time.Minute,
		Logger:               logger,
	})
	return &App{
		cfg:    &cfg,
		paths:  paths,
		log:    logger,
		store:  st,
		local:  rm,
		work:   work,
		docker: docker,
		wt:     runner.NewWorktreeManager(filepath.Join(paths.DataDir, "worktrees")),
	}, nil
}

// Close cleans up resources.
func (a *App) Close() {
	a.store.Close()
}

func (a *App) Store() *store.Store   { return a.store }
func (a *App) Runner() runner.Runner { return a.local }
func (a *App) Config() config.Config { return *a.cfg }
func (a *App) Paths() config.Paths   { return a.paths }
func (a *App) Logger() *slog.Logger  { return a.log }

// UpdateConfig updates the in-memory config safely.
func (a *App) UpdateConfig(cfg config.Config) {
	a.cfg = &cfg
}

// SyncSessions syncs sessions from the disk to the store.
func (a *App) SyncSessions(ctx context.Context) (int, int, error) {
	items, err := session.ScanMany(ctx, a.SessionDirs())
	if err != nil {
		return 0, 0, err
	}
	newWS, newSess := 0, 0
	for _, item := range items {
		ws, created, err := a.store.UpsertWorkspace(ctx, item.WorkspacePath, filepath.Base(item.WorkspacePath))
		if err != nil {
			continue
		}
		if created {
			newWS++
		}
		ok, err := a.store.UpsertSession(ctx, store.Session{
			ID: item.SessionID, WorkspaceID: ws.ID, FilePath: item.FilePath, Title: item.Title,
			CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		})
		if err == nil && ok {
			newSess++
		}
	}
	return newWS, newSess, nil
}

func (a *App) SessionDirs() []string {
	return []string{
		a.cfg.Runner.SessionDir,
		filepath.Join(a.paths.DataDir, "sessions", "worktree"),
		filepath.Join(a.paths.DataDir, "sessions", "docker"),
	}
}

func (a *App) PromptSession(ctx context.Context, sess store.Session, ws store.Workspace, runnerType string, req runner.StartRequest, message string, images []runner.ImageAttachment) (store.Session, error) {
	prepared, r, err := a.prepareRunner(ctx, sess, ws, runnerType, req)
	if err != nil {
		return sess, err
	}
	req.Workspace = runnerWorkspace(prepared, ws)
	if err := r.Prompt(ctx, req, message, images); err != nil {
		return prepared, err
	}
	return prepared, nil
}

func (a *App) SteerSession(ctx context.Context, sess store.Session, ws store.Workspace, req runner.StartRequest, message string, images []runner.ImageAttachment) error {
	runnerType := normalizedRunnerType(sess.RunnerType)
	prepared, r, err := a.prepareRunner(ctx, sess, ws, runnerType, req)
	if err != nil {
		return err
	}
	req.Workspace = runnerWorkspace(prepared, ws)
	return r.Steer(ctx, req, message, images)
}

func (a *App) IsGitWorkspace(ctx context.Context, ws store.Workspace) bool {
	return runner.IsGitWorkspace(ctx, ws.Path)
}

func (a *App) HasDirtyChanges(ctx context.Context, ws store.Workspace) bool {
	return runner.HasDirtyChanges(ctx, ws.Path)
}

func (a *App) prepareRunner(ctx context.Context, sess store.Session, ws store.Workspace, runnerType string, req runner.StartRequest) (store.Session, runner.Runner, error) {
	runnerType = normalizedRunnerType(runnerType)
	if err := a.store.SetSessionRunnerType(ctx, sess.ID, runnerType, ws.Path); err != nil {
		return sess, nil, err
	}
	sess.RunnerType = runnerType
	if sess.OriginalWorkspacePath == "" {
		sess.OriginalWorkspacePath = ws.Path
	}
	switch runnerType {
	case "local":
		return sess, a.local, nil
	case "worktree", "docker":
		meta, _, err := a.wt.Ensure(ctx, sess.ID, ws.Path, runner.WorktreeMeta{
			Path:       sess.WorktreePath,
			Branch:     sess.WorktreeBranch,
			BaseCommit: sess.BaseCommit,
		})
		if err != nil {
			return sess, nil, err
		}
		if sess.WorktreePath != meta.Path || sess.WorktreeBranch != meta.Branch || sess.BaseCommit != meta.BaseCommit {
			if err := a.store.SetSessionWorktree(ctx, sess.ID, meta.Path, meta.Branch, meta.BaseCommit); err != nil {
				return sess, nil, err
			}
		}
		sess.WorktreePath = meta.Path
		sess.WorktreeBranch = meta.Branch
		sess.BaseCommit = meta.BaseCommit
		if runnerType == "docker" {
			return sess, a.docker, nil
		}
		return sess, a.work, nil
	default:
		return sess, nil, nil
	}
}

func runnerWorkspace(sess store.Session, ws store.Workspace) string {
	if normalizedRunnerType(sess.RunnerType) == "local" {
		return ws.Path
	}
	return sess.WorktreePath
}

func normalizedRunnerType(runnerType string) string {
	switch runnerType {
	case "", "local":
		return "local"
	case "worktree", "docker":
		return runnerType
	default:
		return "local"
	}
}

// ResolveModel determines which model to use based on session, workspace, and global defaults.
func (a *App) ResolveModel(sess store.Session, ws store.Workspace) string {
	if sess.Model != "" {
		return sess.Model
	}
	if ws.Model != "" {
		return ws.Model
	}
	return a.cfg.GlobalModel
}

// SetGlobalModel updates the global model setting in the config file.
func (a *App) SetGlobalModel(model string) error {
	return config.SetGlobalModel(a.paths.ConfigPath, model)
}
