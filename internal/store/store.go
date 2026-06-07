package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Workspace struct {
	ID              int64
	Path            string
	Name            string
	Model           string
	Frequency       int
	LatestSessionAt sql.NullTime
	CreatedAt       time.Time
}

type Session struct {
	ID                    string
	WorkspaceID           int64
	FilePath              string
	Name                  string
	Title                 string
	Model                 string
	TopicID               int
	GoalMessageID         int
	RunnerType            string
	OriginalWorkspacePath string
	WorktreePath          string
	WorktreeBranch        string
	BaseCommit            string
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
PRAGMA foreign_keys = ON;
CREATE TABLE IF NOT EXISTS workspaces (
    id INTEGER PRIMARY KEY,
    path TEXT NOT NULL UNIQUE,
    name TEXT,
    model TEXT,
    frequency INTEGER NOT NULL DEFAULT 0,
    latest_session_at DATETIME,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    workspace_id INTEGER NOT NULL REFERENCES workspaces(id),
    file_path TEXT NOT NULL,
    name TEXT,
    title TEXT,
    model TEXT,
    topic_id INTEGER,
    goal_message_id INTEGER,
    runner_type TEXT,
    original_workspace_path TEXT,
    worktree_path TEXT,
    worktree_branch TEXT,
    base_commit TEXT,
    created_at DATETIME,
    updated_at DATETIME
);
CREATE INDEX IF NOT EXISTS idx_sessions_workspace_id ON sessions(workspace_id);
CREATE INDEX IF NOT EXISTS idx_sessions_topic_id ON sessions(topic_id);
`)
	if err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "workspaces", "frequency", `ALTER TABLE workspaces ADD COLUMN frequency INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "workspaces", "latest_session_at", `ALTER TABLE workspaces ADD COLUMN latest_session_at DATETIME`); err != nil {
		return err
	}
	for _, col := range []struct {
		name string
		stmt string
	}{
		{"runner_type", `ALTER TABLE sessions ADD COLUMN runner_type TEXT`},
		{"original_workspace_path", `ALTER TABLE sessions ADD COLUMN original_workspace_path TEXT`},
		{"worktree_path", `ALTER TABLE sessions ADD COLUMN worktree_path TEXT`},
		{"worktree_branch", `ALTER TABLE sessions ADD COLUMN worktree_branch TEXT`},
		{"base_commit", `ALTER TABLE sessions ADD COLUMN base_commit TEXT`},
	} {
		if err := s.ensureColumn(ctx, "sessions", col.name, col.stmt); err != nil {
			return err
		}
	}
	return s.recomputeAllWorkspaceStats(ctx)
}

func (s *Store) ensureColumn(ctx context.Context, table, column, stmt string) error {
	exists, err := s.columnExists(ctx, table, column)
	if err != nil || exists {
		return err
	}
	_, err = s.db.ExecContext(ctx, stmt)
	return err
}

func (s *Store) columnExists(ctx context.Context, table, column string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid        int
			name       string
			colType    string
			notNull    int
			defaultV   sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultV, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) UpsertWorkspace(ctx context.Context, path, name string) (Workspace, bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO workspaces(path, name) VALUES(?, ?)`, path, name)
	if err != nil {
		return Workspace{}, false, err
	}
	rows, _ := res.RowsAffected()
	ws, err := s.GetWorkspaceByPath(ctx, path)
	return ws, rows > 0, err
}

func (s *Store) GetWorkspace(ctx context.Context, id int64) (Workspace, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, path, COALESCE(name,''), COALESCE(model,''), COALESCE(frequency,0), latest_session_at, created_at FROM workspaces WHERE id = ?`, id)
	return scanWorkspace(row)
}

func (s *Store) GetWorkspaceByPath(ctx context.Context, path string) (Workspace, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, path, COALESCE(name,''), COALESCE(model,''), COALESCE(frequency,0), latest_session_at, created_at FROM workspaces WHERE path = ?`, path)
	return scanWorkspace(row)
}

func (s *Store) ListWorkspaces(ctx context.Context, limit, offset int) ([]Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, path, COALESCE(name,''), COALESCE(model,''), COALESCE(frequency,0), latest_session_at, created_at FROM workspaces ORDER BY (COALESCE(frequency,0) * (1.0 / (1.0 + MAX(0, CAST(julianday('now', 'localtime', 'start of day') - julianday(COALESCE(latest_session_at, created_at, 'now'), 'localtime', 'start of day') AS INTEGER))))) DESC, latest_session_at DESC, created_at DESC, path LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		ws, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ws)
	}
	return out, rows.Err()
}

func (s *Store) CountWorkspaces(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workspaces`).Scan(&count)
	return count, err
}

func (s *Store) UpsertSession(ctx context.Context, sess Session) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}

	var previousWorkspaceID int64
	err = tx.QueryRowContext(ctx, `SELECT workspace_id FROM sessions WHERE id = ?`, sess.ID).Scan(&previousWorkspaceID)
	created := false
	switch {
	case errors.Is(err, sql.ErrNoRows):
		created = true
		_, err = tx.ExecContext(ctx, `
INSERT INTO sessions(id, workspace_id, file_path, name, title, model, topic_id, goal_message_id, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, NULLIF(?, 0), NULLIF(?, 0), ?, ?)
`, sess.ID, sess.WorkspaceID, sess.FilePath, sess.Name, sess.Title, sess.Model, sess.TopicID, sess.GoalMessageID, sess.CreatedAt, sess.UpdatedAt)
	case err != nil:
		_ = tx.Rollback()
		return false, err
	default:
		_, err = tx.ExecContext(ctx, `
UPDATE sessions
SET workspace_id = ?,
    file_path = ?,
    name = COALESCE(NULLIF(?, ''), name),
    title = COALESCE(NULLIF(?, ''), title),
    model = COALESCE(NULLIF(?, ''), model),
    created_at = COALESCE(created_at, ?),
    updated_at = ?
WHERE id = ?
`, sess.WorkspaceID, sess.FilePath, sess.Name, sess.Title, sess.Model, sess.CreatedAt, sess.UpdatedAt, sess.ID)
	}
	if err != nil {
		_ = tx.Rollback()
		return false, err
	}
	if err := s.recomputeWorkspaceStatsTx(ctx, tx, sess.WorkspaceID); err != nil {
		_ = tx.Rollback()
		return false, err
	}
	if !created && previousWorkspaceID != 0 && previousWorkspaceID != sess.WorkspaceID {
		if err := s.recomputeWorkspaceStatsTx(ctx, tx, previousWorkspaceID); err != nil {
			_ = tx.Rollback()
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return created, nil
}

func (s *Store) CreatePlaceholderSession(ctx context.Context, workspaceID int64, title string) (Session, error) {
	now := time.Now().UTC()
	id := "pico-" + now.Format("20060102150405.000000000")
	sess := Session{ID: id, WorkspaceID: workspaceID, Title: title, CreatedAt: now, UpdatedAt: now}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(id, workspace_id, file_path, title, created_at, updated_at) VALUES(?, ?, '', ?, ?, ?)`, sess.ID, sess.WorkspaceID, sess.Title, sess.CreatedAt, sess.UpdatedAt); err != nil {
		_ = tx.Rollback()
		return Session{}, err
	}
	if err := s.recomputeWorkspaceStatsTx(ctx, tx, workspaceID); err != nil {
		_ = tx.Rollback()
		return Session{}, err
	}
	if err := tx.Commit(); err != nil {
		return Session{}, err
	}
	return sess, nil
}

func (s *Store) SetSessionRunnerType(ctx context.Context, sessionID, runnerType, originalWorkspacePath string) error {
	if runnerType == "" {
		runnerType = "local"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(runner_type,'') FROM sessions WHERE id = ?`, sessionID).Scan(&existing)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if existing != "" && existing != runnerType {
		_ = tx.Rollback()
		return fmt.Errorf("session %s already uses runner %s", sessionID, existing)
	}
	now := time.Now().UTC()
	_, err = tx.ExecContext(ctx, `
UPDATE sessions
SET runner_type = ?,
    original_workspace_path = COALESCE(NULLIF(original_workspace_path, ''), ?),
    updated_at = ?
WHERE id = ?
`, runnerType, originalWorkspacePath, now, sessionID)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := s.recomputeWorkspaceStatsForSessionTx(ctx, tx, sessionID); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) SetSessionWorktree(ctx context.Context, sessionID, worktreePath, worktreeBranch, baseCommit string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
UPDATE sessions
SET worktree_path = ?,
    worktree_branch = ?,
    base_commit = ?,
    updated_at = ?
WHERE id = ?
`, worktreePath, worktreeBranch, baseCommit, now, sessionID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := s.recomputeWorkspaceStatsForSessionTx(ctx, tx, sessionID); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) ListSessions(ctx context.Context, workspaceID int64, limit, offset int) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, sessionSelectSQL()+` WHERE workspace_id = ? ORDER BY updated_at DESC LIMIT ? OFFSET ?`, workspaceID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		sess, err := scanSessionRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

func (s *Store) CountSessions(ctx context.Context, workspaceID int64) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE workspace_id = ?`, workspaceID).Scan(&count)
	return count, err
}

func (s *Store) GetSession(ctx context.Context, id string) (Session, error) {
	row := s.db.QueryRowContext(ctx, sessionSelectSQL()+` WHERE id = ?`, id)
	return scanSession(row)
}

func (s *Store) GetSessionByTopic(ctx context.Context, topicID int) (Session, error) {
	row := s.db.QueryRowContext(ctx, sessionSelectSQL()+` WHERE topic_id = ?`, topicID)
	return scanSession(row)
}

func (s *Store) SetSessionTopic(ctx context.Context, sessionID string, topicID, goalMessageID int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET topic_id = ?, goal_message_id = ?, updated_at = ? WHERE id = ?`, topicID, goalMessageID, now, sessionID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := s.recomputeWorkspaceStatsForSessionTx(ctx, tx, sessionID); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) SetWorkspaceModel(ctx context.Context, workspaceID int64, model string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE workspaces SET model = ? WHERE id = ?`, model, workspaceID)
	return err
}

func (s *Store) SetSessionModel(ctx context.Context, sessionID, model string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET model = ?, updated_at = ? WHERE id = ?`, model, now, sessionID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := s.recomputeWorkspaceStatsForSessionTx(ctx, tx, sessionID); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) recomputeWorkspaceStatsForSessionTx(ctx context.Context, tx *sql.Tx, sessionID string) error {
	var workspaceID int64
	if err := tx.QueryRowContext(ctx, `SELECT workspace_id FROM sessions WHERE id = ?`, sessionID).Scan(&workspaceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	return s.recomputeWorkspaceStatsTx(ctx, tx, workspaceID)
}

func (s *Store) recomputeWorkspaceStatsTx(ctx context.Context, tx *sql.Tx, workspaceID int64) error {
	_, err := tx.ExecContext(ctx, `
UPDATE workspaces
SET frequency = COALESCE((SELECT COUNT(*) FROM sessions WHERE workspace_id = workspaces.id), 0),
    latest_session_at = (SELECT MAX(updated_at) FROM sessions WHERE workspace_id = workspaces.id)
WHERE id = ?
`, workspaceID)
	return err
}

func (s *Store) recomputeAllWorkspaceStats(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE workspaces
SET frequency = COALESCE((SELECT COUNT(*) FROM sessions WHERE workspace_id = workspaces.id), 0),
    latest_session_at = (SELECT MAX(updated_at) FROM sessions WHERE workspace_id = workspaces.id)
`)
	return err
}

func scanWorkspace(row interface{ Scan(dest ...any) error }) (Workspace, error) {
	var ws Workspace
	err := row.Scan(&ws.ID, &ws.Path, &ws.Name, &ws.Model, &ws.Frequency, &ws.LatestSessionAt, &ws.CreatedAt)
	return ws, err
}

func scanSession(row interface{ Scan(dest ...any) error }) (Session, error) {
	return scanSessionRows(row)
}

func scanSessionRows(row interface{ Scan(dest ...any) error }) (Session, error) {
	var sess Session
	err := row.Scan(
		&sess.ID,
		&sess.WorkspaceID,
		&sess.FilePath,
		&sess.Name,
		&sess.Title,
		&sess.Model,
		&sess.TopicID,
		&sess.GoalMessageID,
		&sess.RunnerType,
		&sess.OriginalWorkspacePath,
		&sess.WorktreePath,
		&sess.WorktreeBranch,
		&sess.BaseCommit,
		&sess.CreatedAt,
		&sess.UpdatedAt,
	)
	return sess, err
}

func sessionSelectSQL() string {
	return `SELECT id,
workspace_id,
file_path,
COALESCE(name,''),
COALESCE(title,''),
COALESCE(model,''),
COALESCE(topic_id,0),
COALESCE(goal_message_id,0),
COALESCE(runner_type,''),
COALESCE(original_workspace_path,''),
COALESCE(worktree_path,''),
COALESCE(worktree_branch,''),
COALESCE(base_commit,''),
created_at,
updated_at FROM sessions`
}

func IsNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}
