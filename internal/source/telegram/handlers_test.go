package telegram

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaot/pi-coordinator/internal/app"
	"github.com/xiaot/pi-coordinator/internal/config"
	"github.com/xiaot/pi-coordinator/internal/store"
)

func TestAwaitRunModeTextIncludesWorkspace(t *testing.T) {
	tmp := t.TempDir()
	cfg := config.Config{}
	cfg.GlobalModel = "opencode-go/deepseek-v4-pro"
	paths := config.Paths{
		DataDir:    tmp,
		DBPath:     filepath.Join(tmp, "test.db"),
		ConfigPath: filepath.Join(tmp, "config.yaml"),
	}
	a, err := app.New(cfg, paths, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	defer a.Close()

	b := NewBot(a)
	sess := store.Session{Title: "hello 20260609"}
	ws := store.Workspace{Path: filepath.Join(tmp, "workspace")}
	text := awaitRunModeText(context.Background(), b, sess, ws)

	if !strings.Contains(text, "Choose a run mode for pi.") {
		t.Fatalf("text should use the updated heading: %q", text)
	}
	if strings.Contains(text, "Topic will be created after you choose the run mode.") {
		t.Fatalf("text should not include deferred topic creation copy: %q", text)
	}
	if !strings.Contains(text, "Workspace: "+ws.Path) {
		t.Fatalf("text should include workspace info: %q", text)
	}
	if !strings.Contains(text, "Model: opencode-go/deepseek-v4-pro") {
		t.Fatalf("text should include resolved model: %q", text)
	}
}
