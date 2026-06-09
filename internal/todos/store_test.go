package todos

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreCreateListUpdateDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "todos.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	first, err := s.Create(42, "", "Alpha\nfirst detail")
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	time.Sleep(time.Millisecond)
	second, err := s.Create(42, "", "Beta\nsecond detail")
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}

	items, err := s.ListByWorkspace(42, "")
	if err != nil {
		t.Fatalf("ListByWorkspace: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0].ID != second.ID {
		t.Fatalf("expected newest item first, got %q", items[0].ID)
	}

	updated, err := s.Update(first.ID, "Gamma\nupdated detail")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Title != "Gamma" {
		t.Fatalf("expected updated title, got %q", updated.Title)
	}

	items, err = s.ListByWorkspace(42, "")
	if err != nil {
		t.Fatalf("ListByWorkspace after update: %v", err)
	}
	if items[0].ID != first.ID {
		t.Fatalf("expected updated item first, got %q", items[0].ID)
	}

	deleted, err := s.Delete(second.ID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if deleted.ID != second.ID {
		t.Fatalf("expected deleted id %q, got %q", second.ID, deleted.ID)
	}

	items, err = s.ListByWorkspace(42, "")
	if err != nil {
		t.Fatalf("ListByWorkspace after delete: %v", err)
	}
	if len(items) != 1 || items[0].ID != first.ID {
		t.Fatalf("unexpected remaining items: %+v", items)
	}
}

func TestStoreTemporaryWorkspaceIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "todos.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.Create(0, WorkspaceKindTemporary, "Temp title\nbody"); err != nil {
		t.Fatalf("Create temporary: %v", err)
	}
	if _, err := s.Create(99, "", "Normal title\nbody"); err != nil {
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
