package telegram

import "testing"

func TestStartedTaskKeyboardIncludesFollowButton(t *testing.T) {
	kb := startedTaskKeyboard(42, false, -1001234567890, 321)
	if len(kb.InlineKeyboard) != 1 {
		t.Fatalf("expected 1 row, got %d", len(kb.InlineKeyboard))
	}
	row := kb.InlineKeyboard[0]
	if len(row) != 4 {
		t.Fatalf("expected 4 buttons, got %d", len(row))
	}
	if row[0].Text != "💬 Follow" {
		t.Fatalf("expected first button to be Follow, got %q", row[0].Text)
	}
	if row[0].URL != "https://t.me/c/1234567890/321" {
		t.Fatalf("unexpected follow URL: %q", row[0].URL)
	}
}

func TestTemporaryStartedTaskKeyboardOnlyShowsFollow(t *testing.T) {
	kb := temporaryStartedTaskKeyboard(-1001234567890, 321)
	if len(kb.InlineKeyboard) != 1 || len(kb.InlineKeyboard[0]) != 1 {
		t.Fatalf("unexpected keyboard shape: %+v", kb)
	}
	if got := kb.InlineKeyboard[0][0].Text; got != "💬 Follow" {
		t.Fatalf("expected follow button, got %q", got)
	}
}

func TestCreatedTopicKeyboardForTemporarySessionOnlyShowsDocker(t *testing.T) {
	kb := createdTopicKeyboard("sess-1", false, true)
	if len(kb.InlineKeyboard) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(kb.InlineKeyboard))
	}
	if len(kb.InlineKeyboard[0]) != 1 {
		t.Fatalf("expected 1 run button, got %d", len(kb.InlineKeyboard[0]))
	}
	if got := kb.InlineKeyboard[0][0].Text; got != "Docker" {
		t.Fatalf("expected Docker button, got %q", got)
	}
	if got := kb.InlineKeyboard[0][0].CallbackData; got != "rundocker:sess-1" {
		t.Fatalf("unexpected callback: %q", got)
	}
}

func TestTodoWorkspaceKeyRoundTrip(t *testing.T) {
	id, temporary, ok := parseTodoWorkspaceKey(todoWorkspaceKey(42, false))
	if !ok || temporary || id != 42 {
		t.Fatalf("unexpected normal workspace parse: id=%d temporary=%v ok=%v", id, temporary, ok)
	}
	id, temporary, ok = parseTodoWorkspaceKey(todoWorkspaceKey(0, true))
	if !ok || !temporary || id != 0 {
		t.Fatalf("unexpected temporary workspace parse: id=%d temporary=%v ok=%v", id, temporary, ok)
	}
}
