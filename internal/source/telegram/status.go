package telegram

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/xiaot/pi-coordinator/internal/app"
	"github.com/xiaot/pi-coordinator/internal/runner"
)

const activeSessionPageSize = 8

func handleStatusCmd(ctx context.Context, b *Bot, update Update) {
	sendActiveSessions(ctx, b, update.Message.Chat.ID, 0, 0, "")
}

func handleStatusCallback(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	data := strings.TrimPrefix(q.Data, "status:")
	switch {
	case data == "refresh":
		sendActiveSessions(ctx, b, q.Message.Chat.ID, q.Message.MessageID, 0, "")
	case strings.HasPrefix(data, "list:"):
		page, _ := strconv.Atoi(strings.TrimPrefix(data, "list:"))
		sendActiveSessions(ctx, b, q.Message.Chat.ID, q.Message.MessageID, page, "")
	case strings.HasPrefix(data, "view:"):
		sessionID, page := parseStatusTarget(strings.TrimPrefix(data, "view:"))
		showActiveSession(ctx, b, q.Message.Chat.ID, q.Message.MessageID, sessionID, page)
	case strings.HasPrefix(data, "stop:"):
		sessionID, page := parseStatusTarget(strings.TrimPrefix(data, "stop:"))
		stopActiveSession(ctx, b, q.Message.Chat.ID, q.Message.MessageID, sessionID, page)
	default:
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Unknown status action.", nil)
	}
}

func sendActiveSessions(ctx context.Context, b *Bot, chatID int64, messageID int, page int, notice string) {
	active, err := b.app.ListActiveSessions(ctx)
	if err != nil {
		b.sendOrEdit(chatID, messageID, "Failed to read active sessions: "+err.Error(), nil)
		return
	}
	text := "Active sessions"
	if notice != "" {
		text = notice + "\n\n" + text
	}
	if len(active) == 0 {
		b.sendOrEdit(chatID, messageID, text+"\n\nNo active sessions.", inlineKeyboardMarkup{InlineKeyboard: [][]inlineKeyboardButton{{{Text: "Refresh", CallbackData: "status:refresh"}}}})
		return
	}
	page = clampPage(page, len(active), activeSessionPageSize)
	start := page * activeSessionPageSize
	end := min(start+activeSessionPageSize, len(active))
	var rows [][]inlineKeyboardButton
	for _, item := range active[start:end] {
		rows = append(rows, inlineKeyboardRow(inlineKeyboardButton{Text: activeSessionLabel(item), CallbackData: "status:view:" + item.Process.SessionID + ":" + strconv.Itoa(page)}))
	}
	rows = appendPageNav(rows, page, len(active), activeSessionPageSize, "status:list:")
	rows = append(rows, inlineKeyboardRow(inlineKeyboardButton{Text: "Refresh", CallbackData: "status:refresh"}))
	b.sendOrEdit(chatID, messageID, fmt.Sprintf("%s (%d)", text, len(active)), inlineKeyboardMarkup{InlineKeyboard: rows})
}

func showActiveSession(ctx context.Context, b *Bot, chatID int64, messageID int, sessionID string, page int) {
	active, err := b.app.ListActiveSessions(ctx)
	if err != nil {
		b.editMessageText(chatID, messageID, "Failed to read active sessions: "+err.Error(), nil)
		return
	}
	for _, item := range active {
		if item.Process.SessionID != sessionID {
			continue
		}
		b.editMessageText(chatID, messageID, activeSessionDetailText(b, item), activeSessionDetailKeyboard(b, item, page))
		return
	}
	sendActiveSessions(ctx, b, chatID, messageID, page, "Session is no longer active.")
}

func stopActiveSession(ctx context.Context, b *Bot, chatID int64, messageID int, sessionID string, page int) {
	title := sessionID
	active, _ := b.app.ListActiveSessions(ctx)
	for _, item := range active {
		if item.Process.SessionID == sessionID {
			title = displaySession(item.Session)
			break
		}
	}
	if err := b.app.StopActiveSession(ctx, sessionID); err != nil && err != runner.ErrSessionNotActive {
		b.editMessageText(chatID, messageID, "Failed to stop session: "+err.Error(), nil)
		return
	}
	sendActiveSessions(ctx, b, chatID, messageID, page, "Stopped session: "+title)
}

func parseStatusTarget(raw string) (string, int) {
	parts := strings.SplitN(raw, ":", 2)
	if len(parts) != 2 {
		return raw, 0
	}
	page, _ := strconv.Atoi(parts[1])
	return parts[0], page
}

func activeSessionLabel(item app.ActiveSession) string {
	state := "⚪️"
	if item.Process.Busy {
		state = "🟢"
	}
	label := statusSessionTitle(item)
	if label == item.Process.SessionID {
		label = trimMiddle(label, 24)
	}
	runnerType := item.RunnerType
	if runnerType == "" {
		runnerType = "local"
	}
	text := fmt.Sprintf("%s %s · %s", state, label, runnerType)
	return trimRunes(text, 60)
}

func activeSessionDetailText(b *Bot, item app.ActiveSession) string {
	now := time.Now()
	status := "idle"
	if item.Process.Busy {
		status = "busy"
	}
	workspace := item.Workspace.Path
	if workspace == "" {
		workspace = "-"
	}
	workspaceName := item.Workspace.Name
	if workspaceName == "" && workspace != "-" {
		workspaceName = filepath.Base(workspace)
	}
	if workspaceName == "" {
		workspaceName = "-"
	}
	model := b.app.ResolveModel(item.Session, item.Workspace)
	if model == "" {
		model = "pi default"
	}
	lines := []string{
		"Session: " + statusSessionTitle(item),
		"Status: " + status,
		"Runner: " + item.RunnerType,
		"Workspace: " + workspaceName,
		"Path: " + workspace,
		"Topic: " + statusTopicLabel(item.Session.TopicID),
		"Model: " + model,
		"PID: " + statusPIDLabel(item.Process.PID),
		"Started: " + statusTimeLabel(now, item.Process.StartedAt),
		"Last activity: " + statusTimeLabel(now, item.Process.LastUsed),
		"Session ID: " + item.Process.SessionID,
	}
	if item.Session.WorktreePath != "" {
		lines = append(lines, "Worktree: "+item.Session.WorktreePath)
	}
	if item.Session.WorktreeBranch != "" {
		lines = append(lines, "Branch: "+item.Session.WorktreeBranch)
	}
	return strings.Join(lines, "\n")
}

func activeSessionDetailKeyboard(b *Bot, item app.ActiveSession, page int) inlineKeyboardMarkup {
	var rows [][]inlineKeyboardButton
	var row []inlineKeyboardButton
	if item.Session.TopicID != 0 {
		row = append(row, inlineKeyboardButton{Text: "Follow", URL: topicURL(b.app.Config().Telegram.GroupChatID, item.Session.TopicID)})
	}
	row = append(row, inlineKeyboardButton{Text: "Stop", CallbackData: "status:stop:" + item.Process.SessionID + ":" + strconv.Itoa(page)})
	rows = append(rows, row)
	rows = append(rows, inlineKeyboardRow(
		inlineKeyboardButton{Text: "< Back", CallbackData: "status:list:" + strconv.Itoa(page)},
		inlineKeyboardButton{Text: "Refresh", CallbackData: "status:view:" + item.Process.SessionID + ":" + strconv.Itoa(page)},
	))
	return inlineKeyboardMarkup{InlineKeyboard: rows}
}

func statusTopicLabel(topicID int) string {
	if topicID == 0 {
		return "-"
	}
	return strconv.Itoa(topicID)
}

func statusPIDLabel(pid int) string {
	if pid == 0 {
		return "-"
	}
	return strconv.Itoa(pid)
}

func statusTimeLabel(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05") + " (" + app.FormatRelativeDuration(now, t) + " ago)"
}

func statusSessionTitle(item app.ActiveSession) string {
	title := strings.TrimSpace(displaySession(item.Session))
	if title == "" {
		return item.Process.SessionID
	}
	return title
}

func trimRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}

func trimMiddle(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	if limit <= 3 {
		return string(runes[:limit])
	}
	left := (limit - 1) / 2
	right := limit - left - 1
	return string(runes[:left]) + "…" + string(runes[len(runes)-right:])
}
