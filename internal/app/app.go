package app

import (
	"context"
	"log/slog"
	"path/filepath"

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
	runner runner.Runner
}

func New(cfg config.Config, paths config.Paths, logger *slog.Logger) (*App, error) {
	st, err := store.Open(paths.DBPath)
	if err != nil {
		return nil, err
	}
	rm := runner.NewLocal(runner.LocalOptions{
		Binary:      cfg.Runner.Binary,
		SessionDir:  cfg.Runner.SessionDir,
		IdleTimeout: cfg.Runner.IdleTimeout.Duration,
		Logger:      logger,
	})
	return &App{
		cfg:    &cfg,
		paths:  paths,
		log:    logger,
		store:  st,
		runner: rm,
	}, nil
}

// Close cleans up resources.
func (a *App) Close() {
	a.store.Close()
}

func (a *App) Store() *store.Store { return a.store }
func (a *App) Runner() runner.Runner { return a.runner }
func (a *App) Config() config.Config { return *a.cfg }
func (a *App) Paths() config.Paths { return a.paths }
func (a *App) Logger() *slog.Logger { return a.log }

// UpdateConfig updates the in-memory config safely.
func (a *App) UpdateConfig(cfg config.Config) {
	a.cfg = &cfg
}

// SyncSessions syncs sessions from the disk to the store.
func (a *App) SyncSessions(ctx context.Context) (int, int, error) {
	items, err := session.Scan(ctx, a.cfg.Runner.SessionDir)
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
