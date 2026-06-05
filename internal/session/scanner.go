package session

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Discovered struct {
	WorkspacePath string
	SessionID     string
	FilePath      string
	Title         string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func Scan(ctx context.Context, root string) ([]Discovered, error) {
	var out []Discovered
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if isIgnoredDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".jsonl" {
			return nil
		}
		item, ok := readSessionFile(path)
		if ok && dirExists(item.WorkspacePath) {
			out = append(out, item)
		}
		return nil
	})
	return out, err
}

func isIgnoredDir(name string) bool {
	lower := strings.ToLower(name)
	return (strings.HasPrefix(lower, "--private-") && strings.HasSuffix(lower, "--")) ||
		strings.Contains(lower, "tmp") ||
		strings.Contains(lower, "temp")
}

func readSessionFile(path string) (Discovered, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Discovered{}, false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var found Discovered
	found.FilePath = path
	for scanner.Scan() {
		var raw map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &raw); err != nil {
			continue
		}
		if ts, ok := parseTimestamp(raw["timestamp"]); ok {
			if found.CreatedAt.IsZero() || ts.Before(found.CreatedAt) {
				found.CreatedAt = ts
			}
			if found.UpdatedAt.IsZero() || ts.After(found.UpdatedAt) {
				found.UpdatedAt = ts
			}
		}
		if raw["type"] == "session" {
			if cwd, _ := raw["cwd"].(string); cwd != "" {
				found.WorkspacePath = cwd
			}
			if id, _ := raw["id"].(string); id != "" {
				found.SessionID = id
			}
			continue
		}
		if found.Title == "" {
			if title := firstUserLine(raw); title != "" {
				found.Title = title
			}
		}
	}
	if found.SessionID == "" || found.WorkspacePath == "" {
		return Discovered{}, false
	}
	if found.Title == "" {
		found.Title = filepath.Base(path)
	}
	if st, err := os.Stat(path); err == nil {
		if found.CreatedAt.IsZero() {
			found.CreatedAt = st.ModTime()
		}
		if found.UpdatedAt.IsZero() {
			found.UpdatedAt = st.ModTime()
		}
	}
	if found.UpdatedAt.IsZero() {
		found.UpdatedAt = found.CreatedAt
	}
	return found, true
}

func parseTimestamp(raw any) (time.Time, bool) {
	ts, _ := raw.(string)
	if ts == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func firstUserLine(raw map[string]any) string {
	message, ok := raw["message"].(map[string]any)
	if !ok || message["role"] != "user" {
		return ""
	}
	content, ok := message["content"].([]any)
	if !ok || len(content) == 0 {
		return ""
	}
	part, ok := content[0].(map[string]any)
	if !ok {
		return ""
	}
	text, _ := part["text"].(string)
	return strings.TrimSpace(strings.Split(text, "\n")[0])
}

func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
