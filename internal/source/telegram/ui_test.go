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
