package telegram

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/xiaot/pi-coordinator/internal/app"
	"github.com/xiaot/pi-coordinator/internal/runner"
)

type Bot struct {
	app    *app.App
	router *Router

	mu          sync.Mutex
	pins        map[int64]string
	pinMessages map[int64][]PinMessage
	pending     map[int64]PendingState
}

type PinMessage struct {
	ChatID    int64
	MessageID int
}

type PendingState struct {
	Kind            string
	WorkspaceID     int64
	SessionID       string
	Prompt          string
	ImagesJSON      string // JSON-serialised []runner.ImageAttachment, for trace retry
	TaskChatID      int64
	TaskChatType    string
	PromptChatID    int64
	PromptMessageID int
	ModelScope      string
	ModelID         string
	Provider        string
}

func NewBot(a *app.App) *Bot {
	b := &Bot{
		app:         a,
		pins:        make(map[int64]string),
		pinMessages: make(map[int64][]PinMessage),
		pending:     make(map[int64]PendingState),
	}
	b.router = NewRouter(b)
	b.registerHandlers()
	return b
}

func (b *Bot) Run(ctx context.Context) error {
	b.app.Logger().Info("telegram bot started")

	b.cleanupStalePinnedWorkspaceMessages()

	if err := b.registerCommands(ctx); err != nil {
		b.app.Logger().Warn("register telegram commands failed", "error", err)
	}

	offset := 0
	for {
		updates, err := b.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			b.app.Logger().Warn("get updates failed", "error", err)
			time.Sleep(time.Second)
			continue
		}
		for _, update := range updates {
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
			b.router.HandleUpdate(ctx, update)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

func (b *Bot) allowed(userID int64) bool {
	for _, id := range b.app.Config().Telegram.AllowedUsers {
		if id == userID {
			return true
		}
	}
	return false
}

// -- State Management --

func (b *Bot) setPending(userID int64, p PendingState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending[userID] = p
}

func (b *Bot) getPending(userID int64) (PendingState, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.pending[userID]
	return p, ok
}

func (b *Bot) clearPending(userID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pending, userID)
}

func (b *Bot) hasPendingPromptReply(userID, chatID int64, replyToMessageID int) bool {
	if replyToMessageID == 0 {
		return false
	}
	p, ok := b.getPending(userID)
	return ok &&
		(p.Kind == "await_new_prompt" || p.Kind == "await_resume_prompt") &&
		p.PromptChatID == chatID &&
		p.PromptMessageID == replyToMessageID
}

func (b *Bot) pinned(userID int64) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pins[userID]
}

func (b *Bot) setPin(userID int64, path string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pins[userID] = path
}

func (b *Bot) clearPin(userID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pins, userID)
}

func (b *Bot) trackPinMessage(userID, chatID int64, messageID int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pinMessages[userID] = append(b.pinMessages[userID], PinMessage{ChatID: chatID, MessageID: messageID})
}

func (b *Bot) trackedPinMessages(userID int64) []PinMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	messages := b.pinMessages[userID]
	return append([]PinMessage(nil), messages...)
}

func (b *Bot) clearTrackedPinMessages(userID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pinMessages, userID)
}

func (b *Bot) trackedPinUsers() []int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	users := make([]int64, 0, len(b.pinMessages))
	for userID := range b.pinMessages {
		users = append(users, userID)
	}
	return users
}

func (b *Bot) clearUserPin(ctx context.Context, userID int64) {
	b.clearPin(userID)
	b.unpinTrackedMessages(ctx, userID)
}

func (b *Bot) CleanupPins(ctx context.Context) {
	for _, userID := range b.trackedPinUsers() {
		b.clearUserPin(ctx, userID)
	}
}

func (b *Bot) unpinTrackedMessages(ctx context.Context, userID int64) {
	for _, message := range b.trackedPinMessages(userID) {
		if err := b.unpinChatMessage(ctx, message.ChatID, message.MessageID); err != nil {
			b.app.Logger().Warn("telegram unpin message failed", "chat_id", message.ChatID, "message_id", message.MessageID, "error", err)
		}
	}
	b.clearTrackedPinMessages(userID)
}

// -- Telegram API Helpers --

func (b *Bot) callTelegramCtx(ctx context.Context, method string, payload map[string]any, out any, timeout time.Duration) error {
	return b.callTelegramWithTokenCtx(ctx, b.app.Config().Telegram.BotToken, method, payload, out, timeout)
}

func (b *Bot) callTelegramWithTokenCtx(ctx context.Context, token, method string, payload map[string]any, out any, timeout time.Duration) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := "https://api.telegram.org/bot" + token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("telegram %s returned %s", method, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (b *Bot) callTelegram(method string, payload map[string]any, out any) error {
	return b.callTelegramCtx(context.Background(), method, payload, out, 20*time.Second)
}

func (b *Bot) getUpdates(ctx context.Context, offset int) ([]Update, error) {
	var resp struct {
		OK          bool     `json:"ok"`
		Result      []Update `json:"result"`
		Description string   `json:"description"`
	}
	err := b.callTelegramCtx(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": 30, "allowed_updates": []string{"message", "callback_query", "managed_bot"},
	}, &resp, 40*time.Second)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, errors.New(resp.Description)
	}
	return resp.Result, nil
}

func (b *Bot) registerCommands(ctx context.Context) error {
	var resp telegramOK
	err := b.callTelegramCtx(ctx, "setMyCommands", map[string]any{
		"commands": []map[string]string{
			{"command": "help", "description": "Show help"},
			{"command": "workspace", "description": "Choose a workspace and session"},
			{"command": "new", "description": "Start a new task"},
			{"command": "sync", "description": "Import historical sessions"},
			{"command": "pin", "description": "Pin a workspace"},
			{"command": "unpin", "description": "Clear the pinned workspace"},
			{"command": "model", "description": "Configure model settings"},
			{"command": "bots", "description": "Show managed role bots"},
		},
	}, &resp, 20*time.Second)
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Description)
	}
	return nil
}

func (b *Bot) send(chatID int64, text string, replyMarkup any) {
	if _, err := b.sendMessage(chatID, 0, text, replyMarkup); err != nil {
		b.app.Logger().Warn("telegram send failed", "error", err)
	}
}

func (b *Bot) sendOrEdit(chatID int64, messageID int, text string, replyMarkup any) {
	if messageID == 0 {
		b.send(chatID, text, replyMarkup)
		return
	}
	if err := b.editMessageText(chatID, messageID, text, replyMarkup); err != nil {
		b.app.Logger().Warn("telegram edit failed", "error", err)
	}
}

func (b *Bot) sendMessage(chatID int64, topicID int, text string, replyMarkup any) (int, error) {
	var resp struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
		Description string `json:"description"`
	}
	payload := map[string]any{"chat_id": chatID, "text": text}
	if topicID != 0 {
		payload["message_thread_id"] = topicID
	}
	if replyMarkup != nil {
		payload["reply_markup"] = replyMarkup
	}
	if err := b.callTelegram("sendMessage", payload, &resp); err != nil {
		return 0, err
	}
	if !resp.OK {
		return 0, errors.New(resp.Description)
	}
	return resp.Result.MessageID, nil
}

func (b *Bot) getChat(ctx context.Context, chatID int64) (*Chat, error) {
	var resp struct {
		OK          bool   `json:"ok"`
		Result      Chat   `json:"result"`
		Description string `json:"description"`
	}
	if err := b.callTelegramCtx(ctx, "getChat", map[string]any{"chat_id": chatID}, &resp, 20*time.Second); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, errors.New(resp.Description)
	}
	return &resp.Result, nil
}

func (b *Bot) editMessageText(chatID int64, messageID int, text string, replyMarkup any) error {
	var resp telegramOK
	payload := map[string]any{"chat_id": chatID, "message_id": messageID, "text": text}
	if replyMarkup != nil {
		payload["reply_markup"] = replyMarkup
	}
	if err := b.callTelegram("editMessageText", payload, &resp); err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Description)
	}
	return nil
}

func (b *Bot) createForumTopic(chatID int64, name string) (int, error) {
	var resp struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageThreadID int `json:"message_thread_id"`
		} `json:"result"`
		Description string `json:"description"`
	}
	if err := b.callTelegram("createForumTopic", map[string]any{"chat_id": chatID, "name": name}, &resp); err != nil {
		return 0, err
	}
	if !resp.OK {
		return 0, errors.New(resp.Description)
	}
	return resp.Result.MessageThreadID, nil
}

func (b *Bot) sendTopicMessage(chatID int64, topicID int, text string, replyMarkup any) (int, error) {
	return b.sendMessage(chatID, topicID, text, replyMarkup)
}

func (b *Bot) pinChatMessage(chatID int64, messageID int) error {
	var resp telegramOK
	err := b.callTelegram("pinChatMessage", map[string]any{
		"chat_id": chatID, "message_id": messageID, "disable_notification": true,
	}, &resp)
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Description)
	}
	return nil
}

func (b *Bot) unpinChatMessage(ctx context.Context, chatID int64, messageID int) error {
	var resp telegramOK
	err := b.callTelegramCtx(ctx, "unpinChatMessage", map[string]any{
		"chat_id": chatID, "message_id": messageID,
	}, &resp, 20*time.Second)
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Description)
	}
	return nil
}

func (b *Bot) cleanupStalePinnedWorkspaceMessages() {
	b.cleanupStalePinnedWorkspaceMessagesInChat(b.app.Config().Telegram.GroupChatID, true)
	for _, chatID := range b.app.Config().Telegram.AllowedUsers {
		b.cleanupStalePinnedWorkspaceMessagesInChat(chatID, false)
	}
}

func (b *Bot) cleanupStalePinnedWorkspaceMessagesInChat(chatID int64, warnOnFailure bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for range 10 {
		chat, err := b.getChat(ctx, chatID)
		if err != nil {
			if warnOnFailure {
				b.app.Logger().Warn("telegram stale pin check failed", "chat_id", chatID, "error", err)
			} else {
				b.app.Logger().Debug("telegram stale pin check skipped", "chat_id", chatID, "error", err)
			}
			return
		}
		if chat.PinnedMessage == nil || !isPinnedWorkspaceText(chat.PinnedMessage.Text) {
			return
		}
		if err := b.unpinChatMessage(ctx, chatID, chat.PinnedMessage.MessageID); err != nil {
			b.app.Logger().Warn("telegram stale pin cleanup failed", "chat_id", chatID, "message_id", chat.PinnedMessage.MessageID, "error", err)
			return
		}
	}
}

func (b *Bot) answerCallback(id, text string) {
	var resp telegramOK
	_ = b.callTelegram("answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, &resp)
}

// -- Telegram Types --

type Update struct {
	UpdateID      int                `json:"update_id"`
	Message       *Message           `json:"message"`
	CallbackQuery *CallbackQuery     `json:"callback_query"`
	ManagedBot    *ManagedBotUpdated `json:"managed_bot"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	From    *User    `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

type Message struct {
	MessageID         int                `json:"message_id"`
	MessageThreadID   int                `json:"message_thread_id"`
	From              *User              `json:"from"`
	Chat              Chat               `json:"chat"`
	Text              string             `json:"text"`
	Caption           string             `json:"caption"`
	Photo             []PhotoSize        `json:"photo"`
	ReplyToMessage    *Message           `json:"reply_to_message"`
	ManagedBotCreated *ManagedBotCreated `json:"managed_bot_created"`
}

type PhotoSize struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FileSize     int64  `json:"file_size"`
}

type User struct {
	ID            int64  `json:"id"`
	IsBot         bool   `json:"is_bot"`
	FirstName     string `json:"first_name"`
	Username      string `json:"username"`
	CanManageBots bool   `json:"can_manage_bots"`
}

type Chat struct {
	ID            int64    `json:"id"`
	Type          string   `json:"type"`
	PinnedMessage *Message `json:"pinned_message"`
}

func (c Chat) IsPrivate() bool {
	return c.Type == "private"
}

func (m *Message) IsCommand() bool {
	return strings.HasPrefix(strings.TrimSpace(m.Text), "/")
}

func (m *Message) Command() string {
	text := strings.TrimSpace(m.Text)
	if !strings.HasPrefix(text, "/") {
		return ""
	}
	cmd := strings.Fields(text)[0]
	cmd = strings.TrimPrefix(cmd, "/")
	if i := strings.Index(cmd, "@"); i >= 0 {
		cmd = cmd[:i]
	}
	return cmd
}

func (m *Message) CommandArguments() string {
	text := strings.TrimSpace(m.Text)
	parts := strings.SplitN(text, " ", 2)
	if len(parts) != 2 {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

type telegramOK struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
}

type ManagedBotCreated struct {
	RequestID int   `json:"request_id"`
	Bot       *User `json:"bot"`
}

type ManagedBotUpdated struct {
	User *User `json:"user"`
	Bot  *User `json:"bot"`
}

// -- Message helpers --

func hasContent(msg *Message) bool {
	return strings.TrimSpace(msg.Text) != "" ||
		strings.TrimSpace(msg.Caption) != "" ||
		len(msg.Photo) > 0
}

func effectiveText(msg *Message) string {
	if t := strings.TrimSpace(msg.Text); t != "" {
		return t
	}
	return strings.TrimSpace(msg.Caption)
}

// -- Image extraction --

func (b *Bot) extractImages(ctx context.Context, msg *Message) ([]runner.ImageAttachment, error) {
	if len(msg.Photo) == 0 {
		return nil, nil
	}
	// Use the largest photo size (last in the array).
	largest := msg.Photo[len(msg.Photo)-1]
	data, err := b.downloadFile(ctx, largest.FileID)
	if err != nil {
		return nil, fmt.Errorf("download photo: %w", err)
	}
	mimeType := "image/jpeg" // Telegram photos are always JPEG.
	return []runner.ImageAttachment{{
		Type:     "image",
		Data:     base64.StdEncoding.EncodeToString(data),
		MimeType: mimeType,
	}}, nil
}

func (b *Bot) downloadFile(ctx context.Context, fileID string) ([]byte, error) {
	var fileResp struct {
		OK     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
	}
	if err := b.callTelegramCtx(ctx, "getFile", map[string]any{"file_id": fileID}, &fileResp, 20*time.Second); err != nil {
		return nil, err
	}
	if !fileResp.OK {
		return nil, errors.New("getFile: telegram returned not ok")
	}

	url := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", b.app.Config().Telegram.BotToken, fileResp.Result.FilePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("download file returned %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func encodeImagesJSON(images []runner.ImageAttachment) string {
	if len(images) == 0 {
		return ""
	}
	data, err := json.Marshal(images)
	if err != nil {
		return ""
	}
	return string(data)
}

func decodeImagesJSON(raw string) []runner.ImageAttachment {
	if raw == "" {
		return nil
	}
	var images []runner.ImageAttachment
	if err := json.Unmarshal([]byte(raw), &images); err != nil {
		return nil
	}
	return images
}
