package app

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xiaot/pi-coordinator/internal/config"
	"github.com/xiaot/pi-coordinator/internal/runner"
	"github.com/xiaot/pi-coordinator/internal/session"
	"github.com/xiaot/pi-coordinator/internal/store"
	"github.com/xiaot/pi-coordinator/internal/todos"
)

const (
	temporaryWorkspacePath = "pico://temporary-workspace"
	temporaryWorkspaceName = "Temporary session"
)

// App is the core coordinator for pi.
// It manages workspaces, sessions, and coordinates with the pi runner.
type App struct {
	cfg    *config.Config // Kept as a pointer; note: config hot-reload may require atomic/mutex if we want dynamic reloading in App
	paths  config.Paths
	log    *slog.Logger
	store  *store.Store
	todos  *todos.Store
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
	todoStore, err := todos.Open(filepath.Join(filepath.Dir(paths.ConfigPath), "todos.json"))
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	pluginDir := filepath.Join(paths.DataDir, "agent")
	dockerPluginDir := pluginDir
	workSessionDir := cfg.Runner.Worktree.SessionDir
	if workSessionDir == "" {
		workSessionDir = filepath.Join(paths.DataDir, "sessions", "worktree")
	}
	dockerSessionDir := filepath.Join(paths.DataDir, "sessions", "docker")
	dockerAgentDir := cfg.Runner.Docker.AgentDir
	if dockerAgentDir == "" {
		home, _ := os.UserHomeDir()
		dockerAgentDir = filepath.Join(home, ".pi", "agent")
	}
	dockerSkillsDir := cfg.Runner.Docker.SkillsDir
	if dockerSkillsDir == "" {
		home, _ := os.UserHomeDir()
		dockerSkillsDir = filepath.Join(home, ".agents", "skills")
	}
	localOpts := runner.LocalOptions{
		Binary:               "pi",
		SessionDir:           cfg.Runner.Local.SessionDir,
		IdleTimeout:          cfg.Runner.Local.IdleTimeout.Duration,
		Plugins:              cfg.Plugins,
		PluginAgentDir:       pluginDir,
		PluginUpdateInterval: time.Duration(cfg.PluginUpdateIntervalMinutes) * time.Minute,
		Logger:               logger,
	}
	rm := runner.NewLocal(localOpts)
	work := runner.NewWorktreeRunner(runner.WorktreeOptions{LocalOptions: localOpts, SessionDir: workSessionDir, Logger: logger})
	docker := runner.NewDocker(runner.DockerOptions{
		Binary:               "pi",
		Image:                cfg.Runner.Docker.Image,
		Network:              cfg.Runner.Docker.Network,
		AgentMountMode:       cfg.Runner.Docker.AgentMountMode,
		ExtraMounts:          dockerMounts(cfg.Runner.Docker.ExtraMounts),
		HostAgentDir:         dockerAgentDir,
		HostPluginDir:        dockerPluginDir,
		HostSkillsDir:        dockerSkillsDir,
		HostSessionDir:       dockerSessionDir,
		IdleTimeout:          cfg.Runner.Docker.IdleTimeout.Duration,
		Plugins:              cfg.Plugins,
		PluginUpdateInterval: time.Duration(cfg.PluginUpdateIntervalMinutes) * time.Minute,
		Logger:               logger,
	})
	return &App{
		cfg:    &cfg,
		paths:  paths,
		log:    logger,
		store:  st,
		todos:  todoStore,
		local:  rm,
		work:   work,
		docker: docker,
		wt:     runner.NewWorktreeManager(filepath.Join(paths.DataDir, "worktrees")),
	}, nil
}

func dockerMounts(mounts config.DockerMounts) []runner.DockerMount {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]runner.DockerMount, 0, len(mounts))
	for _, mount := range mounts {
		out = append(out, runner.DockerMount{HostPath: mount.Host, Mode: mount.Mode, HomeSubpath: mount.HomeSubpath, HomeMapped: mount.HomeMapped})
	}
	return out
}

// Close cleans up resources.
func (a *App) Close() {
	a.store.Close()
}

func (a *App) Store() *store.Store     { return a.store }
func (a *App) TodoStore() *todos.Store { return a.todos }
func (a *App) Runner() runner.Runner   { return a.local }
func (a *App) Config() config.Config   { return *a.cfg }
func (a *App) Paths() config.Paths     { return a.paths }
func (a *App) Logger() *slog.Logger    { return a.log }

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
	workDir := a.cfg.Runner.Worktree.SessionDir
	if workDir == "" {
		workDir = filepath.Join(a.paths.DataDir, "sessions", "worktree")
	}
	dockerDir := filepath.Join(a.paths.DataDir, "sessions", "docker")
	return []string{
		a.cfg.Runner.Local.SessionDir,
		workDir,
		dockerDir,
	}
}

func (a *App) ManagedWorktreeRoot() string {
	return filepath.Join(a.paths.DataDir, "worktrees")
}

func (a *App) IsManagedWorktreePath(path string) bool {
	path = filepath.Clean(path)
	root := filepath.Clean(a.ManagedWorktreeRoot())
	if path == root {
		return true
	}
	return len(path) > len(root) && path[:len(root)] == root && path[len(root)] == filepath.Separator
}

func (a *App) EnsureTemporaryWorkspace(ctx context.Context) (store.Workspace, error) {
	ws, _, err := a.store.UpsertWorkspace(ctx, temporaryWorkspacePath, temporaryWorkspaceName)
	return ws, err
}

func (a *App) IsTemporaryWorkspace(ws store.Workspace) bool {
	return isTemporaryWorkspacePath(ws.Path)
}

func isTemporaryWorkspacePath(path string) bool {
	return strings.TrimSpace(path) == temporaryWorkspacePath
}

func (a *App) CountSelectableWorkspaces(ctx context.Context) (int, error) {
	workspaces, err := a.selectableWorkspaces(ctx)
	if err != nil {
		return 0, err
	}
	return len(workspaces), nil
}

func (a *App) ListSelectableWorkspaces(ctx context.Context, limit, offset int) ([]store.Workspace, error) {
	workspaces, err := a.selectableWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	return paginateWorkspaces(workspaces, limit, offset), nil
}

func (a *App) GetSelectableWorkspace(ctx context.Context, id int64) (store.Workspace, error) {
	ws, err := a.store.GetWorkspace(ctx, id)
	if err != nil {
		return store.Workspace{}, err
	}
	if a.IsManagedWorktreePath(ws.Path) || a.IsTemporaryWorkspace(ws) {
		return store.Workspace{}, sql.ErrNoRows
	}
	return ws, nil
}

func (a *App) GetSelectableWorkspaceByPath(ctx context.Context, path string) (store.Workspace, error) {
	ws, err := a.store.GetWorkspaceByPath(ctx, path)
	if err != nil {
		return store.Workspace{}, err
	}
	if a.IsManagedWorktreePath(ws.Path) || a.IsTemporaryWorkspace(ws) {
		return store.Workspace{}, sql.ErrNoRows
	}
	return ws, nil
}

func (a *App) CountWorkspaceSessions(ctx context.Context, workspaceID int64) (int, error) {
	workspaceIDs, err := a.associatedWorkspaceIDs(ctx, workspaceID)
	if err != nil {
		return 0, err
	}
	return a.store.CountSessionsByWorkspaceIDs(ctx, workspaceIDs)
}

func (a *App) ListWorkspaceSessions(ctx context.Context, workspaceID int64, limit, offset int) ([]store.Session, error) {
	workspaceIDs, err := a.associatedWorkspaceIDs(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	return a.store.ListSessionsByWorkspaceIDs(ctx, workspaceIDs, limit, offset)
}

func (a *App) associatedWorkspaceIDs(ctx context.Context, workspaceID int64) ([]int64, error) {
	selected, err := a.GetSelectableWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	workspaces, err := a.allWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	selectedBase := filepath.Base(filepath.Clean(selected.Path))
	prefix := selectedBase + "_"
	ids := []int64{selected.ID}
	seen := map[int64]bool{selected.ID: true}
	for _, ws := range workspaces {
		if seen[ws.ID] || !a.IsManagedWorktreePath(ws.Path) {
			continue
		}
		hiddenBase := filepath.Base(filepath.Clean(ws.Path))
		if !strings.HasPrefix(hiddenBase, prefix) {
			continue
		}
		ids = append(ids, ws.ID)
		seen[ws.ID] = true
	}
	return ids, nil
}

func (a *App) selectableWorkspaces(ctx context.Context) ([]store.Workspace, error) {
	workspaces, err := a.allWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]store.Workspace, 0, len(workspaces))
	for _, ws := range workspaces {
		if a.IsManagedWorktreePath(ws.Path) || a.IsTemporaryWorkspace(ws) {
			continue
		}
		out = append(out, ws)
	}
	return out, nil
}

func (a *App) allWorkspaces(ctx context.Context) ([]store.Workspace, error) {
	total, err := a.store.CountWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	if total == 0 {
		return nil, nil
	}
	return a.store.ListWorkspaces(ctx, total, 0)
}

func paginateWorkspaces(workspaces []store.Workspace, limit, offset int) []store.Workspace {
	if offset >= len(workspaces) {
		return nil
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		return workspaces[offset:]
	}
	end := offset + limit
	if end > len(workspaces) {
		end = len(workspaces)
	}
	return workspaces[offset:end]
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
	temporary := a.IsTemporaryWorkspace(ws)
	if temporary && runnerType != "docker" {
		return sess, nil, fmt.Errorf("temporary sessions only support docker")
	}
	originalWorkspacePath := ws.Path
	if temporary {
		originalWorkspacePath = ""
	}
	if err := a.store.SetSessionRunnerType(ctx, sess.ID, runnerType, originalWorkspacePath); err != nil {
		return sess, nil, err
	}
	sess.RunnerType = runnerType
	if sess.OriginalWorkspacePath == "" {
		sess.OriginalWorkspacePath = originalWorkspacePath
	}
	if temporary {
		return sess, a.docker, nil
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
	if isTemporaryWorkspacePath(ws.Path) {
		return ""
	}
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
