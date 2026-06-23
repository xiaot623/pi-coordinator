package crons

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreCreateListToggleDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crons.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	first, err := s.Create(CreateInput{WorkspaceID: 42, Prompt: "Alpha\nbody", Schedule: "0 9 * * *", Mode: ModeAuto, Runner: RunnerLocal})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	time.Sleep(time.Millisecond)
	second, err := s.Create(CreateInput{WorkspaceID: 42, Prompt: "Beta\nbody", Schedule: "*/5 * * * *", Mode: ModeManual, Runner: RunnerDocker})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}

	items, err := s.ListByWorkspace(42, "")
	if err != nil {
		t.Fatalf("ListByWorkspace: %v", err)
	}
	if len(items) != 2 || items[0].ID != second.ID {
		t.Fatalf("unexpected order: %+v", items)
	}

	disabled, err := s.SetEnabled(first.ID, false)
	if err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if disabled.Enabled {
		t.Fatalf("expected disabled")
	}

	enabled, err := s.ListEnabled()
	if err != nil {
		t.Fatalf("ListEnabled: %v", err)
	}
	if len(enabled) != 1 || enabled[0].ID != second.ID {
		t.Fatalf("unexpected enabled: %+v", enabled)
	}

	deleted, err := s.Delete(second.ID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if deleted.ID != second.ID {
		t.Fatalf("expected deleted id %q, got %q", second.ID, deleted.ID)
	}
}

func TestStoreTemporaryWorkspaceIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crons.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.Create(CreateInput{WorkspaceKind: WorkspaceKindTemporary, Prompt: "Temp", Schedule: "0 9 * * *", Mode: ModeAuto, Runner: RunnerDocker}); err != nil {
		t.Fatalf("Create temporary: %v", err)
	}
	if _, err := s.Create(CreateInput{WorkspaceID: 99, Prompt: "Normal", Schedule: "0 9 * * *", Mode: ModeAuto, Runner: RunnerLocal}); err != nil {
		t.Fatalf("Create normal: %v", err)
	}
	temporary, err := s.ListByWorkspace(0, WorkspaceKindTemporary)
	if err != nil {
		t.Fatalf("List temporary: %v", err)
	}
	if len(temporary) != 1 || !temporary[0].IsTemporary() {
		t.Fatalf("unexpected temporary items: %+v", temporary)
	}
	normal, err := s.ListByWorkspace(99, "")
	if err != nil {
		t.Fatalf("List normal: %v", err)
	}
	if len(normal) != 1 || normal[0].IsTemporary() {
		t.Fatalf("unexpected normal items: %+v", normal)
	}
}

func TestScheduleMatchesAndNext(t *testing.T) {
	tm := time.Date(2026, 6, 23, 9, 0, 30, 0, time.Local)
	ok, err := Matches("0 9 * * *", tm)
	if err != nil {
		t.Fatalf("Matches: %v", err)
	}
	if !ok {
		t.Fatalf("expected schedule to match")
	}
	ok, err = Matches("*/15 9 * * *", time.Date(2026, 6, 23, 9, 30, 0, 0, time.Local))
	if err != nil || !ok {
		t.Fatalf("expected stepped schedule to match, ok=%v err=%v", ok, err)
	}
	next, err := Next("0 9 * * *", time.Date(2026, 6, 23, 9, 0, 0, 0, time.Local))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	want := time.Date(2026, 6, 24, 9, 0, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Fatalf("expected %v, got %v", want, next)
	}
}

func TestScheduleRejectsNonFiveField(t *testing.T) {
	if _, err := Parse("0 0 9 * * *"); err == nil {
		t.Fatalf("expected 6-field cron to be rejected")
	}
}
