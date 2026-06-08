package telegram

import (
	"strings"
	"testing"

	"github.com/xiaot/pi-coordinator/internal/store"
)

func TestSessionStopWarningTargetPrefersSessionTopic(t *testing.T) {
	chatID, topicID := sessionStopWarningTarget(-1001234567890, store.Session{TopicID: 321}, 999, 654)
	if chatID != -1001234567890 || topicID != 321 {
		t.Fatalf("unexpected target: chat=%d topic=%d", chatID, topicID)
	}
}

func TestSessionStopWarningTargetFallsBackWhenTopicMissing(t *testing.T) {
	chatID, topicID := sessionStopWarningTarget(-1001234567890, store.Session{}, 999, 654)
	if chatID != 999 || topicID != 654 {
		t.Fatalf("unexpected fallback target: chat=%d topic=%d", chatID, topicID)
	}
}

func TestSessionInterruptedWarningTextUsesTitleWhenAvailable(t *testing.T) {
	text := sessionInterruptedWarningText(store.Session{Title: "Fix login flow"})
	if !strings.Contains(text, "Fix login flow was interrupted by user request.") {
		t.Fatalf("unexpected warning text: %q", text)
	}
}

func TestSessionInterruptedWarningTextFallsBackToGenericLabel(t *testing.T) {
	text := sessionInterruptedWarningText(store.Session{})
	if !strings.Contains(text, "This session was interrupted by user request.") {
		t.Fatalf("unexpected generic warning text: %q", text)
	}
}
