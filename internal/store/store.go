package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Workspace struct {
	ID        int64
	Path      string
	Name      string
	Model     string
	CreatedAt time.Time
}

type Session struct {
	ID            string
	WorkspaceID   int64
	FilePath      string
	Name          string
	Title         string
	Model         string
	TopicID       int
	GoalMessageID int
	CreatedAt     time.Time
	UpdatedAt     time.Time
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
    created_at DATETIME,
    updated_at DATETIME
);
CREATE INDEX IF NOT EXISTS idx_sessions_workspace_id ON sessions(workspace_id);
CREATE INDEX IF NOT EXISTS idx_sessions_topic_id ON sessions(topic_id);
`)
	return err
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
	row := s.db.QueryRowContext(ctx, `SELECT id, path, COALESCE(name,''), COALESCE(model,''), created_at FROM workspaces WHERE id = ?`, id)
	return scanWorkspace(row)
}

func (s *Store) GetWorkspaceByPath(ctx context.Context, path string) (Workspace, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, path, COALESCE(name,''), COALESCE(model,''), created_at FROM workspaces WHERE path = ?`, path)
	return scanWorkspace(row)
}

func (s *Store) ListWorkspaces(ctx context.Context, limit, offset int) ([]Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, path, COALESCE(name,''), COALESCE(model,''), created_at FROM workspaces ORDER BY path LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		var ws Workspace
		if err := rows.Scan(&ws.ID, &ws.Path, &ws.Name, &ws.Model, &ws.CreatedAt); err != nil {
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
	res, err := s.db.ExecContext(ctx, `
INSERT INTO sessions(id, workspace_id, file_path, name, title, model, topic_id, goal_message_id, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, NULLIF(?, 0), NULLIF(?, 0), ?, ?)
ON CONFLICT(id) DO UPDATE SET
  workspace_id=excluded.workspace_id,
  file_path=excluded.file_path,
  title=COALESCE(NULLIF(excluded.title, ''), sessions.title),
  updated_at=excluded.updated_at
`, sess.ID, sess.WorkspaceID, sess.FilePath, sess.Name, sess.Title, sess.Model, sess.TopicID, sess.GoalMessageID, sess.CreatedAt, sess.UpdatedAt)
	if err != nil {
		return false, err
	}
	rows, _ := res.RowsAffected()
	return rows > 0, nil
}

func (s *Store) CreatePlaceholderSession(ctx context.Context, workspaceID int64, title string) (Session, error) {
	now := time.Now().UTC()
	id := "pico-" + now.Format("20060102150405.000000000")
	sess := Session{ID: id, WorkspaceID: workspaceID, Title: title, CreatedAt: now, UpdatedAt: now}
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions(id, workspace_id, file_path, title, created_at, updated_at) VALUES(?, ?, '', ?, ?, ?)`, sess.ID, sess.WorkspaceID, sess.Title, sess.CreatedAt, sess.UpdatedAt)
	return sess, err
}

func (s *Store) ListSessions(ctx context.Context, workspaceID int64, limit, offset int) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, workspace_id, file_path, COALESCE(name,''), COALESCE(title,''), COALESCE(model,''), COALESCE(topic_id,0), COALESCE(goal_message_id,0), created_at, updated_at FROM sessions WHERE workspace_id = ? ORDER BY updated_at DESC LIMIT ? OFFSET ?`, workspaceID, limit, offset)
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
	row := s.db.QueryRowContext(ctx, `SELECT id, workspace_id, file_path, COALESCE(name,''), COALESCE(title,''), COALESCE(model,''), COALESCE(topic_id,0), COALESCE(goal_message_id,0), created_at, updated_at FROM sessions WHERE id = ?`, id)
	return scanSession(row)
}

func (s *Store) GetSessionByTopic(ctx context.Context, topicID int) (Session, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, workspace_id, file_path, COALESCE(name,''), COALESCE(title,''), COALESCE(model,''), COALESCE(topic_id,0), COALESCE(goal_message_id,0), created_at, updated_at FROM sessions WHERE topic_id = ?`, topicID)
	return scanSession(row)
}

func (s *Store) SetSessionTopic(ctx context.Context, sessionID string, topicID, goalMessageID int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET topic_id = ?, goal_message_id = ?, updated_at = ? WHERE id = ?`, topicID, goalMessageID, time.Now().UTC(), sessionID)
	return err
}

func (s *Store) SetWorkspaceModel(ctx context.Context, workspaceID int64, model string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE workspaces SET model = ? WHERE id = ?`, model, workspaceID)
	return err
}

func (s *Store) SetSessionModel(ctx context.Context, sessionID, model string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET model = ?, updated_at = ? WHERE id = ?`, model, time.Now().UTC(), sessionID)
	return err
}

func scanWorkspace(row interface{ Scan(dest ...any) error }) (Workspace, error) {
	var ws Workspace
	err := row.Scan(&ws.ID, &ws.Path, &ws.Name, &ws.Model, &ws.CreatedAt)
	return ws, err
}

func scanSession(row interface{ Scan(dest ...any) error }) (Session, error) {
	return scanSessionRows(row)
}

func scanSessionRows(row interface{ Scan(dest ...any) error }) (Session, error) {
	var sess Session
	err := row.Scan(&sess.ID, &sess.WorkspaceID, &sess.FilePath, &sess.Name, &sess.Title, &sess.Model, &sess.TopicID, &sess.GoalMessageID, &sess.CreatedAt, &sess.UpdatedAt)
	return sess, err
}

func IsNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}
