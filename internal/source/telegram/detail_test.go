package telegram

import (
	"testing"

	"github.com/xiaot/pi-coordinator/internal/gitops"
)

func TestParseDetailGitReport(t *testing.T) {
	result := gitops.Result{
		Values: map[string]string{
			"GIT_AVAILABLE": "1",
			"REPO_ROOT":     "/repo",
			"BRANCH":        "feat/detail",
			"HEAD":          "a1b2c3d",
			"WORKING_TREE":  "2 staged · 3 unstaged · 1 untracked",
			"DIFF_STAT":     "5 files changed, +82 -17",
		},
		Stdout: "RESULT=ok\nFILE=internal/source/telegram/handlers.go\t34\t12\nFILE=docs/detail.md\t0\t0\tuntracked\n",
	}
	report := parseDetailGitReport(result)
	if !report.Available {
		t.Fatal("expected git report to be available")
	}
	if got := len(report.Files); got != 2 {
		t.Fatalf("expected 2 files, got %d", got)
	}
	if report.Files[0].Path != "internal/source/telegram/handlers.go" || report.Files[0].Additions != 34 || report.Files[0].Deletions != 12 {
		t.Fatalf("unexpected first file: %+v", report.Files[0])
	}
	if report.Files[1].Status != "untracked" {
		t.Fatalf("expected untracked status, got %+v", report.Files[1])
	}
}

func TestDetailKeyboardCurrentTab(t *testing.T) {
	kb := detailKeyboard(detailTabGit)
	if len(kb.InlineKeyboard) != 1 || len(kb.InlineKeyboard[0]) != 3 {
		t.Fatalf("unexpected keyboard shape: %+v", kb)
	}
	if got := kb.InlineKeyboard[0][1].Text; got != "•Git" {
		t.Fatalf("expected current git tab label, got %q", got)
	}
	if got := kb.InlineKeyboard[0][2].CallbackData; got != "detail:refresh:git" {
		t.Fatalf("expected git refresh callback, got %q", got)
	}
}
