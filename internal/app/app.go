package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xiaot/pi-coordinator/internal/config"
	"github.com/xiaot/pi-coordinator/internal/runner"
	"github.com/xiaot/pi-coordinator/internal/session"
	"github.com/xiaot/pi-coordinator/internal/store"
)

type App struct {
	cfg    config.Config
	paths  config.Paths
	log    *slog.Logger
	store  *store.Store
	runner *runner.Manager

	mu      sync.Mutex
	pins    map[int64]string
	pending map[int64]pending
}

type pending struct {
	Kind        string
	WorkspaceID int64
	SessionID   string
	Prompt      string
}

func New(cfg config.Config, paths config.Paths, logger *slog.Logger) (*App, error) {
	st, err := store.Open(paths.DBPath)
	if err != nil {
		return nil, err
	}
	rm := runner.NewManager(runner.Options{
		Binary:      cfg.Runner.Binary,
		SessionDir:  cfg.Runner.SessionDir,
		BotToken:    cfg.Telegram.BotToken,
		GroupChatID: cfg.Telegram.GroupChatID,
		IdleTimeout: cfg.Runner.IdleTimeout.Duration,
		Logger:      logger,
	})
	return &App{
		cfg: cfg, paths: paths, log: logger, store: st, runner: rm,
		pins: map[int64]string{}, pending: map[int64]pending{},
	}, nil
}

func (a *App) Run(ctx context.Context) error {
	defer a.store.Close()
	a.log.Info("pico started", "db", a.paths.DBPath)
	offset := 0
	for {
		updates, err := a.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			a.log.Warn("get updates failed", "error", err)
			time.Sleep(time.Second)
			continue
		}
		for _, update := range updates {
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
			a.handleUpdate(ctx, update)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

func (a *App) handleUpdate(ctx context.Context, update tgUpdate) {
	if update.CallbackQuery != nil {
		a.handleCallback(ctx, update.CallbackQuery)
		return
	}
	if update.Message == nil || update.Message.From == nil || update.Message.From.IsBot {
		return
	}
	if !a.allowed(update.Message.From.ID) {
		return
	}
	msg := update.Message
	if msg.Chat.IsPrivate() {
		a.handlePrivateMessage(ctx, msg)
		return
	}
	if msg.Chat.ID == a.cfg.Telegram.GroupChatID && msg.MessageThreadID != 0 && strings.TrimSpace(msg.Text) != "" {
		a.handleTopicMessage(ctx, msg)
	}
}

func (a *App) handlePrivateMessage(ctx context.Context, msg *tgMessage) {
	if msg.IsCommand() {
		switch msg.Command() {
		case "start":
			a.send(msg.Chat.ID, "pico 已就绪。使用 /sync 同步历史会话，或 /new 发起新任务。", nil)
		case "sync":
			a.syncSessions(ctx, msg.Chat.ID)
		case "workspace":
			a.clearPending(msg.From.ID)
			a.sendWorkspaces(ctx, msg.Chat.ID, "选择 workspace：", "w:")
		case "new":
			a.setPending(msg.From.ID, pending{Kind: "new_prompt", Prompt: strings.TrimSpace(msg.CommandArguments())})
			a.sendWorkspaces(ctx, msg.Chat.ID, "选择 workspace：", "newws:")
		case "unpin":
			a.mu.Lock()
			delete(a.pins, msg.From.ID)
			a.mu.Unlock()
			a.send(msg.Chat.ID, "已取消固定工作目录。", nil)
		case "model":
			a.send(msg.Chat.ID, "/model 的交互入口已预留；当前可先在配置或数据库中设置 provider/modelId。", nil)
		default:
			a.send(msg.Chat.ID, "未知命令。可用：/workspace /new /sync /unpin /model", nil)
		}
		return
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}
	if p, ok := a.getPending(msg.From.ID); ok {
		switch p.Kind {
		case "await_new_prompt":
			a.clearPending(msg.From.ID)
			a.startNewTask(ctx, msg.Chat.ID, msg.From.ID, p.WorkspaceID, text)
			return
		case "await_resume_prompt":
			a.clearPending(msg.From.ID)
			a.resumeSession(ctx, msg.Chat.ID, p.SessionID, text)
			return
		}
	}
	if path := a.pinned(msg.From.ID); path != "" {
		ws, err := a.ensureWorkspace(ctx, path)
		if err != nil {
			a.send(msg.Chat.ID, "固定目录不可用："+err.Error(), nil)
			return
		}
		a.startNewTask(ctx, msg.Chat.ID, msg.From.ID, ws.ID, text)
		return
	}
	a.send(msg.Chat.ID, "请先使用 /new 或 /workspace 选择工作目录。", nil)
}

func (a *App) handleCallback(ctx context.Context, q *tgCallbackQuery) {
	if q.From == nil || !a.allowed(q.From.ID) {
		return
	}
	a.answerCallback(q.ID, "")
	data := q.Data
	chatID := q.Message.Chat.ID
	switch {
	case strings.HasPrefix(data, "w:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "w:"), 10, 64)
		a.sendSessions(ctx, chatID, id)
	case strings.HasPrefix(data, "newws:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "newws:"), 10, 64)
		p, _ := a.getPending(q.From.ID)
		if p.Prompt != "" {
			a.clearPending(q.From.ID)
			a.startNewTask(ctx, chatID, q.From.ID, id, p.Prompt)
			return
		}
		a.setPending(q.From.ID, pending{Kind: "await_new_prompt", WorkspaceID: id})
		a.send(chatID, "请输入任务描述。", nil)
	case strings.HasPrefix(data, "ns:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "ns:"), 10, 64)
		a.setPending(q.From.ID, pending{Kind: "await_new_prompt", WorkspaceID: id})
		a.send(chatID, "请输入任务描述。", nil)
	case strings.HasPrefix(data, "s:"):
		sid := strings.TrimPrefix(data, "s:")
		a.setPending(q.From.ID, pending{Kind: "await_resume_prompt", SessionID: sid})
		a.send(chatID, "请输入要继续发送给该 session 的消息。", nil)
	case strings.HasPrefix(data, "sessions:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "sessions:"), 10, 64)
		a.sendSessions(ctx, chatID, id)
	case strings.HasPrefix(data, "new:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "new:"), 10, 64)
		a.setPending(q.From.ID, pending{Kind: "await_new_prompt", WorkspaceID: id})
		a.send(chatID, "请输入任务描述。", nil)
	case strings.HasPrefix(data, "pin:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "pin:"), 10, 64)
		ws, err := a.store.GetWorkspace(ctx, id)
		if err != nil {
			a.send(chatID, "固定失败："+err.Error(), nil)
			return
		}
		a.mu.Lock()
		a.pins[q.From.ID] = ws.Path
		a.mu.Unlock()
		a.send(chatID, fmt.Sprintf("📌 已固定工作目录：%s\n后续所有消息将在此目录下直接创建新任务。\n/unpin 或重新 /workspace 可取消。", ws.Path), nil)
	}
}

func (a *App) handleTopicMessage(ctx context.Context, msg *tgMessage) {
	sess, err := a.store.GetSessionByTopic(ctx, msg.MessageThreadID)
	if err != nil {
		return
	}
	ws, err := a.store.GetWorkspace(ctx, sess.WorkspaceID)
	if err != nil {
		a.replyTopic(msg, "找不到 session 对应的 workspace。")
		return
	}
	req := runner.StartRequest{
		SessionID: sess.ID, Title: displaySession(sess), Workspace: ws.Path, TopicID: sess.TopicID,
		Model: a.resolveModel(sess, ws), Existing: true,
	}
	if err := a.runner.Steer(ctx, req, msg.Text); err != nil {
		a.replyTopic(msg, "发送给 pi 失败："+err.Error())
	}
}

func (a *App) syncSessions(ctx context.Context, chatID int64) {
	items, err := session.Scan(ctx, a.cfg.Runner.SessionDir)
	if err != nil {
		a.send(chatID, "同步失败："+err.Error(), nil)
		return
	}
	newWS, newSess := 0, 0
	for _, item := range items {
		ws, created, err := a.store.UpsertWorkspace(ctx, item.WorkspacePath, filepath.Base(item.WorkspacePath))
		if err != nil {
			continue
		}
		if created {
			newWS++
		}
		ok, err := a.store.UpsertSession(ctx, store.Session{
			ID: item.SessionID, WorkspaceID: ws.ID, FilePath: item.FilePath, Title: item.Title,
			CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		})
		if err == nil && ok {
			newSess++
		}
	}
	a.send(chatID, fmt.Sprintf("同步完成：发现 %d 个新 workspace，%d 个 session", newWS, newSess), nil)
}

func (a *App) sendWorkspaces(ctx context.Context, chatID int64, text, prefix string) {
	workspaces, err := a.store.ListWorkspaces(ctx)
	if err != nil {
		a.send(chatID, "读取 workspace 失败："+err.Error(), nil)
		return
	}
	if len(workspaces) == 0 {
		a.send(chatID, "还没有 workspace。请先运行 /sync。", nil)
		return
	}
	var rows [][]inlineKeyboardButton
	for _, ws := range workspaces {
		label := ws.Name
		if label == "" {
			label = filepath.Base(ws.Path)
		}
		rows = append(rows, inlineKeyboardRow(inlineKeyboardButton{Text: label, CallbackData: prefix + strconv.FormatInt(ws.ID, 10)}))
	}
	a.send(chatID, text, inlineKeyboardMarkup{InlineKeyboard: rows})
}

func (a *App) sendSessions(ctx context.Context, chatID int64, workspaceID int64) {
	sessions, err := a.store.ListSessions(ctx, workspaceID)
	if err != nil {
		a.send(chatID, "读取 sessions 失败："+err.Error(), nil)
		return
	}
	var rows [][]inlineKeyboardButton
	rows = append(rows, inlineKeyboardRow(inlineKeyboardButton{Text: "+ New Session", CallbackData: "ns:" + strconv.FormatInt(workspaceID, 10)}))
	for _, sess := range sessions {
		rows = append(rows, inlineKeyboardRow(inlineKeyboardButton{Text: displaySession(sess), CallbackData: "s:" + sess.ID}))
	}
	a.send(chatID, "选择 session：", inlineKeyboardMarkup{InlineKeyboard: rows})
}

func (a *App) startNewTask(ctx context.Context, chatID, userID int64, workspaceID int64, prompt string) {
	ws, err := a.store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		a.send(chatID, "读取 workspace 失败："+err.Error(), nil)
		return
	}
	title := topicTitle(prompt)
	topicID, err := a.createForumTopic(title)
	if err != nil {
		a.send(chatID, "创建 Topic 失败："+err.Error(), nil)
		return
	}
	goalID, err := a.sendTopicMessage(topicID, "🎯 "+prompt, taskKeyboard(workspaceID, a.pinned(userID) == ws.Path))
	if err == nil {
		_ = a.pinChatMessage(goalID)
	}
	sess, err := a.store.CreatePlaceholderSession(ctx, workspaceID, title)
	if err != nil {
		a.send(chatID, "创建 session 失败："+err.Error(), nil)
		return
	}
	_ = a.store.SetSessionTopic(ctx, sess.ID, topicID, goalID)
	req := runner.StartRequest{SessionID: sess.ID, Title: title, Workspace: ws.Path, TopicID: topicID, Model: a.resolveModel(sess, ws)}
	if err := a.runner.Prompt(ctx, req, prompt); err != nil {
		a.send(chatID, "启动 pi 失败："+err.Error(), nil)
		return
	}
	a.send(chatID, fmt.Sprintf("已创建 Topic：%s", title), nil)
}

func (a *App) resumeSession(ctx context.Context, chatID int64, sessionID, prompt string) {
	sess, err := a.store.GetSession(ctx, sessionID)
	if err != nil {
		a.send(chatID, "读取 session 失败："+err.Error(), nil)
		return
	}
	ws, err := a.store.GetWorkspace(ctx, sess.WorkspaceID)
	if err != nil {
		a.send(chatID, "读取 workspace 失败："+err.Error(), nil)
		return
	}
	if sess.TopicID == 0 {
		topicID, err := a.createForumTopic(topicTitle(prompt))
		if err != nil {
			a.send(chatID, "创建 Topic 失败："+err.Error(), nil)
			return
		}
		goalID, _ := a.sendTopicMessage(topicID, "🎯 "+prompt, taskKeyboard(ws.ID, false))
		_ = a.pinChatMessage(goalID)
		_ = a.store.SetSessionTopic(ctx, sess.ID, topicID, goalID)
		sess.TopicID = topicID
	}
	req := runner.StartRequest{SessionID: sess.ID, Title: displaySession(sess), Workspace: ws.Path, TopicID: sess.TopicID, Model: a.resolveModel(sess, ws), Existing: true}
	if err := a.runner.Prompt(ctx, req, prompt); err != nil {
		a.send(chatID, "启动 pi 失败："+err.Error(), nil)
		return
	}
	a.send(chatID, "已发送到 session。", nil)
}

func (a *App) ensureWorkspace(ctx context.Context, path string) (store.Workspace, error) {
	ws, err := a.store.GetWorkspaceByPath(ctx, path)
	if err == nil {
		return ws, nil
	}
	if store.IsNotFound(err) || err == sql.ErrNoRows {
		ws, _, err := a.store.UpsertWorkspace(ctx, path, filepath.Base(path))
		return ws, err
	}
	return store.Workspace{}, err
}

func (a *App) resolveModel(sess store.Session, ws store.Workspace) string {
	if sess.Model != "" {
		return sess.Model
	}
	if ws.Model != "" {
		return ws.Model
	}
	return a.cfg.GlobalModel
}

func (a *App) allowed(userID int64) bool {
	for _, id := range a.cfg.Telegram.AllowedUsers {
		if id == userID {
			return true
		}
	}
	return false
}

func (a *App) send(chatID int64, text string, replyMarkup any) {
	if _, err := a.sendMessage(chatID, 0, text, replyMarkup); err != nil {
		a.log.Warn("telegram send failed", "error", err)
	}
}

func (a *App) replyTopic(msg *tgMessage, text string) {
	_, _ = a.sendMessage(msg.Chat.ID, msg.MessageThreadID, text, nil)
}

func (a *App) answerCallback(id, text string) {
	var resp telegramOK
	_ = a.callTelegram("answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, &resp)
}

func (a *App) createForumTopic(name string) (int, error) {
	var resp struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageThreadID int `json:"message_thread_id"`
		} `json:"result"`
		Description string `json:"description"`
	}
	if err := a.callTelegram("createForumTopic", map[string]any{"chat_id": a.cfg.Telegram.GroupChatID, "name": name}, &resp); err != nil {
		return 0, err
	}
	if !resp.OK {
		return 0, errors.New(resp.Description)
	}
	return resp.Result.MessageThreadID, nil
}

func (a *App) sendTopicMessage(topicID int, text string, replyMarkup any) (int, error) {
	return a.sendMessage(a.cfg.Telegram.GroupChatID, topicID, text, replyMarkup)
}

func (a *App) sendMessage(chatID int64, topicID int, text string, replyMarkup any) (int, error) {
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
	if err := a.callTelegram("sendMessage", payload, &resp); err != nil {
		return 0, err
	}
	if !resp.OK {
		return 0, errors.New(resp.Description)
	}
	return resp.Result.MessageID, nil
}

func (a *App) pinChatMessage(messageID int) error {
	var resp telegramOK
	err := a.callTelegram("pinChatMessage", map[string]any{
		"chat_id": a.cfg.Telegram.GroupChatID, "message_id": messageID, "disable_notification": true,
	}, &resp)
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Description)
	}
	return nil
}

func (a *App) getUpdates(ctx context.Context, offset int) ([]tgUpdate, error) {
	var resp struct {
		OK          bool       `json:"ok"`
		Result      []tgUpdate `json:"result"`
		Description string     `json:"description"`
	}
	err := a.callTelegramCtx(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": 30, "allowed_updates": []string{"message", "callback_query"},
	}, &resp, 40*time.Second)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, errors.New(resp.Description)
	}
	return resp.Result, nil
}

func (a *App) callTelegram(method string, payload map[string]any, out any) error {
	return a.callTelegramCtx(context.Background(), method, payload, out, 20*time.Second)
}

func (a *App) callTelegramCtx(ctx context.Context, method string, payload map[string]any, out any, timeout time.Duration) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := "https://api.telegram.org/bot" + a.cfg.Telegram.BotToken + "/" + method
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

func (a *App) setPending(userID int64, p pending) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending[userID] = p
}

func (a *App) getPending(userID int64) (pending, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.pending[userID]
	return p, ok
}

func (a *App) clearPending(userID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.pending, userID)
}

func (a *App) pinned(userID int64) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pins[userID]
}

func taskKeyboard(workspaceID int64, pinned bool) inlineKeyboardMarkup {
	pinLabel := "📌 Pin"
	if pinned {
		pinLabel = "📍 Unpin"
	}
	id := strconv.FormatInt(workspaceID, 10)
	return inlineKeyboardMarkup{InlineKeyboard: [][]inlineKeyboardButton{{
		{Text: "🆕 New", CallbackData: "new:" + id},
		{Text: "📋 Sessions", CallbackData: "sessions:" + id},
		{Text: pinLabel, CallbackData: "pin:" + id},
	}}}
}

func displaySession(sess store.Session) string {
	title := sess.Name
	if title == "" {
		title = sess.Title
	}
	if title == "" {
		title = sess.ID
	}
	if len([]rune(title)) > 50 {
		title = string([]rune(title)[:50])
	}
	return title
}

func topicTitle(prompt string) string {
	line := strings.TrimSpace(strings.Split(prompt, "\n")[0])
	if line == "" {
		line = "New task"
	}
	runes := []rune(line)
	if len(runes) > 128 {
		return string(runes[:128])
	}
	return line
}

type tgUpdate struct {
	UpdateID      int              `json:"update_id"`
	Message       *tgMessage       `json:"message"`
	CallbackQuery *tgCallbackQuery `json:"callback_query"`
}

type tgCallbackQuery struct {
	ID      string     `json:"id"`
	From    *tgUser    `json:"from"`
	Message *tgMessage `json:"message"`
	Data    string     `json:"data"`
}

type tgMessage struct {
	MessageID       int     `json:"message_id"`
	MessageThreadID int     `json:"message_thread_id"`
	From            *tgUser `json:"from"`
	Chat            tgChat  `json:"chat"`
	Text            string  `json:"text"`
}

type tgUser struct {
	ID    int64 `json:"id"`
	IsBot bool  `json:"is_bot"`
}

type tgChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

func (c tgChat) IsPrivate() bool {
	return c.Type == "private"
}

func (m *tgMessage) IsCommand() bool {
	return strings.HasPrefix(strings.TrimSpace(m.Text), "/")
}

func (m *tgMessage) Command() string {
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

func (m *tgMessage) CommandArguments() string {
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

type inlineKeyboardMarkup struct {
	InlineKeyboard [][]inlineKeyboardButton `json:"inline_keyboard"`
}

type inlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

func inlineKeyboardRow(buttons ...inlineKeyboardButton) []inlineKeyboardButton {
	return buttons
}
