package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/xiaot/pi-coordinator/internal/runner"
	"github.com/xiaot/pi-coordinator/internal/store"
)

func handleStopCmd(ctx context.Context, b *Bot, update Update) {
	msg := update.Message
	sess, _, ok := sessionTopicCommandContext(ctx, b, msg, "/stop")
	if !ok {
		return
	}
	alreadyInactive, err := stopSessionWithWarning(ctx, b, sess, msg.Chat.ID, msg.MessageThreadID)
	if err != nil {
		b.sendMessage(msg.Chat.ID, msg.MessageThreadID, "Failed to stop session: "+err.Error(), nil)
		return
	}
	if alreadyInactive {
		b.sendMessage(msg.Chat.ID, msg.MessageThreadID, "Session is not active.", nil)
	}
}

func stopSessionWithWarning(ctx context.Context, b *Bot, sess store.Session, fallbackChatID int64, fallbackTopicID int) (bool, error) {
	err := b.app.StopActiveSession(ctx, sess.ID)
	if errors.Is(err, runner.ErrSessionNotActive) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	chatID, topicID := sessionStopWarningTarget(b.app.Config().Telegram.GroupChatID, sess, fallbackChatID, fallbackTopicID)
	if _, err := b.sendMessage(chatID, topicID, sessionInterruptedWarningText(sess), nil); err != nil {
		return false, fmt.Errorf("session stopped but failed to send interruption warning: %w", err)
	}
	return false, nil
}

func sessionStopWarningTarget(groupChatID int64, sess store.Session, fallbackChatID int64, fallbackTopicID int) (int64, int) {
	if sess.TopicID != 0 {
		return groupChatID, sess.TopicID
	}
	return fallbackChatID, fallbackTopicID
}

func sessionInterruptedWarningText(sess store.Session) string {
	title := strings.TrimSpace(sess.Name)
	if title == "" {
		title = strings.TrimSpace(sess.Title)
	}
	if title == "" {
		return "⚠️ This session was interrupted by user request.\nSend a new message to resume from the last saved state."
	}
	return fmt.Sprintf("⚠️ %s was interrupted by user request.\nSend a new message to resume from the last saved state.", title)
}
