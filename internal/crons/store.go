package crons

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	WorkspaceKindTemporary = "temporary"
	ModeAuto               = "auto"
	ModeManual             = "manual"
	RunnerLocal            = "local"
	RunnerWorktree         = "worktree"
	RunnerDocker           = "docker"
)

var (
	ErrNotFound      = errors.New("cron not found")
	ErrEmptyPrompt   = errors.New("cron prompt is empty")
	ErrInvalidMode   = errors.New("invalid cron mode")
	ErrInvalidRunner = errors.New("invalid cron runner")
)

type Item struct {
	ID              string    `json:"id"`
	WorkspaceKind   string    `json:"workspace_kind,omitempty"`
	WorkspaceID     int64     `json:"workspace_id,omitempty"`
	Title           string    `json:"title"`
	Prompt          string    `json:"prompt"`
	Schedule        string    `json:"schedule"`
	Mode            string    `json:"mode"`
	Runner          string    `json:"runner"`
	Enabled         bool      `json:"enabled"`
	LastTriggeredAt time.Time `json:"last_triggered_at,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func (i Item) IsTemporary() bool {
	return strings.TrimSpace(i.WorkspaceKind) == WorkspaceKindTemporary
}

type CreateInput struct {
	WorkspaceID   int64
	WorkspaceKind string
	Prompt        string
	Schedule      string
	Mode          string
	Runner        string
}

type fileData struct {
	Items []Item `json:"items"`
}

type Store struct {
	path string
	mu   sync.Mutex
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: path}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := s.save(fileData{}); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) ListByWorkspace(workspaceID int64, workspaceKind string) ([]Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.load()
	if err != nil {
		return nil, err
	}
	var out []Item
	for _, item := range data.Items {
		if workspaceKind == WorkspaceKindTemporary {
			if item.IsTemporary() {
				out = append(out, item)
			}
			continue
		}
		if !item.IsTemporary() && item.WorkspaceID == workspaceID {
			out = append(out, item)
		}
	}
	sortItems(out)
	return out, nil
}

func (s *Store) ListEnabled() ([]Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.load()
	if err != nil {
		return nil, err
	}
	var out []Item
	for _, item := range data.Items {
		if item.Enabled {
			out = append(out, item)
		}
	}
	sortItems(out)
	return out, nil
}

func (s *Store) Get(id string) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.load()
	if err != nil {
		return Item{}, err
	}
	for _, item := range data.Items {
		if item.ID == id {
			return item, nil
		}
	}
	return Item{}, ErrNotFound
}

func (s *Store) Create(input CreateInput) (Item, error) {
	prompt := strings.TrimSpace(input.Prompt)
	if prompt == "" {
		return Item{}, ErrEmptyPrompt
	}
	schedule := strings.TrimSpace(input.Schedule)
	if _, err := Parse(schedule); err != nil {
		return Item{}, err
	}
	mode := normalizeMode(input.Mode)
	if mode == "" {
		return Item{}, ErrInvalidMode
	}
	runner := normalizeRunner(input.Runner)
	if runner == "" {
		return Item{}, ErrInvalidRunner
	}
	now := time.Now().UTC()
	item := Item{
		ID:            "cron-" + strconv.FormatInt(now.UnixNano(), 36),
		WorkspaceKind: strings.TrimSpace(input.WorkspaceKind),
		WorkspaceID:   input.WorkspaceID,
		Title:         titleFromPrompt(prompt),
		Prompt:        prompt,
		Schedule:      schedule,
		Mode:          mode,
		Runner:        runner,
		Enabled:       true,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if item.IsTemporary() {
		item.WorkspaceID = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.load()
	if err != nil {
		return Item{}, err
	}
	data.Items = append(data.Items, item)
	if err := s.save(data); err != nil {
		return Item{}, err
	}
	return item, nil
}

func (s *Store) UpdatePrompt(id, prompt string) (Item, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return Item{}, ErrEmptyPrompt
	}
	return s.update(id, func(item *Item) error {
		item.Prompt = prompt
		item.Title = titleFromPrompt(prompt)
		return nil
	})
}

func (s *Store) SetEnabled(id string, enabled bool) (Item, error) {
	return s.update(id, func(item *Item) error {
		item.Enabled = enabled
		return nil
	})
}

func (s *Store) MarkTriggered(id string, at time.Time) (Item, error) {
	return s.update(id, func(item *Item) error {
		item.LastTriggeredAt = at.UTC()
		return nil
	})
}

func (s *Store) Delete(id string) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.load()
	if err != nil {
		return Item{}, err
	}
	for i := range data.Items {
		if data.Items[i].ID != id {
			continue
		}
		item := data.Items[i]
		data.Items = append(data.Items[:i], data.Items[i+1:]...)
		if err := s.save(data); err != nil {
			return Item{}, err
		}
		return item, nil
	}
	return Item{}, ErrNotFound
}

func (s *Store) update(id string, fn func(*Item) error) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.load()
	if err != nil {
		return Item{}, err
	}
	for i := range data.Items {
		if data.Items[i].ID != id {
			continue
		}
		if err := fn(&data.Items[i]); err != nil {
			return Item{}, err
		}
		data.Items[i].UpdatedAt = time.Now().UTC()
		if err := s.save(data); err != nil {
			return Item{}, err
		}
		return data.Items[i], nil
	}
	return Item{}, ErrNotFound
}

func (s *Store) load() (fileData, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return fileData{}, nil
	}
	if err != nil {
		return fileData{}, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return fileData{}, nil
	}
	var out fileData
	if err := json.Unmarshal(data, &out); err != nil {
		return fileData{}, err
	}
	return out, nil
}

func (s *Store) save(data fileData) error {
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, s.path)
}

func normalizeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ModeAuto:
		return ModeAuto
	case ModeManual:
		return ModeManual
	default:
		return ""
	}
}

func normalizeRunner(runner string) string {
	switch strings.ToLower(strings.TrimSpace(runner)) {
	case RunnerLocal:
		return RunnerLocal
	case RunnerWorktree:
		return RunnerWorktree
	case RunnerDocker:
		return RunnerDocker
	default:
		return ""
	}
}

func titleFromPrompt(prompt string) string {
	line := strings.TrimSpace(strings.Split(prompt, "\n")[0])
	if line == "" {
		return "Untitled Cron"
	}
	return line
}

func sortItems(items []Item) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Enabled != items[j].Enabled {
			return items[i].Enabled
		}
		if !items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
			return items[i].UpdatedAt.After(items[j].UpdatedAt)
		}
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].ID > items[j].ID
	})
}

func ValidateRunner(runner string) error {
	if normalizeRunner(runner) == "" {
		return fmt.Errorf("%w: %s", ErrInvalidRunner, runner)
	}
	return nil
}
