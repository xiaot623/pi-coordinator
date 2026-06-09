package todos

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const WorkspaceKindTemporary = "temporary"

var (
	ErrNotFound    = errors.New("todo not found")
	ErrEmptyDetail = errors.New("todo detail is empty")
)

type Item struct {
	ID            string    `json:"id"`
	WorkspaceKind string    `json:"workspace_kind,omitempty"`
	WorkspaceID   int64     `json:"workspace_id,omitempty"`
	Title         string    `json:"title"`
	Detail        string    `json:"detail"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (i Item) IsTemporary() bool {
	return strings.TrimSpace(i.WorkspaceKind) == WorkspaceKindTemporary
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

func (s *Store) Create(workspaceID int64, workspaceKind, detail string) (Item, error) {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return Item{}, ErrEmptyDetail
	}
	now := time.Now().UTC()
	item := Item{
		ID:            "todo-" + strconv.FormatInt(now.UnixNano(), 36),
		WorkspaceKind: strings.TrimSpace(workspaceKind),
		WorkspaceID:   workspaceID,
		Title:         titleFromDetail(detail),
		Detail:        detail,
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

func (s *Store) Update(id, detail string) (Item, error) {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return Item{}, ErrEmptyDetail
	}
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
		data.Items[i].Detail = detail
		data.Items[i].Title = titleFromDetail(detail)
		data.Items[i].UpdatedAt = time.Now().UTC()
		if err := s.save(data); err != nil {
			return Item{}, err
		}
		return data.Items[i], nil
	}
	return Item{}, ErrNotFound
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

func titleFromDetail(detail string) string {
	line := strings.TrimSpace(strings.Split(detail, "\n")[0])
	if line == "" {
		return "Untitled Todo"
	}
	return line
}

func sortItems(items []Item) {
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
			return items[i].UpdatedAt.After(items[j].UpdatedAt)
		}
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].ID > items[j].ID
	})
}
