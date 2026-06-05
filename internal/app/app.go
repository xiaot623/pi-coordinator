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
	"sort"
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
	runner runner.Runner

	mu      sync.Mutex
	pins    map[int64]string
	pending map[int64]pending
}

type pending struct {
	Kind        string
	WorkspaceID int64
	SessionID   string
	Prompt      string
	ModelScope  string
	ModelID     string
	Provider    string
}

const (
	workspacePageSize = 10
	sessionPageSize   = 8
)

func New(cfg config.Config, paths config.Paths, logger *slog.Logger) (*App, error) {
	st, err := store.Open(paths.DBPath)
	if err != nil {
		return nil, err
	}
	rm := runner.NewLocal(runner.LocalOptions{
		Binary:      cfg.Runner.Binary,
		SessionDir:  cfg.Runner.SessionDir,
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
	if err := a.registerCommands(ctx); err != nil {
		a.log.Warn("register telegram commands failed", "error", err)
	}
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
			a.send(msg.Chat.ID, "pico is ready. Use /sync to import history, or /new to start a task.", nil)
		case "sync":
			a.syncSessions(ctx, msg.Chat.ID)
		case "workspace":
			a.clearPending(msg.From.ID)
			a.sendWorkspaces(ctx, msg.Chat.ID, "Choose a workspace:", "w:", 0)
		case "new":
			a.setPending(msg.From.ID, pending{Kind: "new_prompt", Prompt: strings.TrimSpace(msg.CommandArguments())})
			a.sendWorkspaces(ctx, msg.Chat.ID, "Choose a workspace:", "newws:", 0)
		case "unpin":
			a.mu.Lock()
			delete(a.pins, msg.From.ID)
			a.mu.Unlock()
			a.send(msg.Chat.ID, "Workspace pin cleared.", nil)
		case "model":
			a.sendModelScopes(ctx, msg.Chat.ID, msg.From.ID, strings.TrimSpace(msg.CommandArguments()))
		default:
			a.send(msg.Chat.ID, "Unknown command. Available: /workspace /new /sync /unpin /model", nil)
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
			a.resumeSession(ctx, msg.Chat.ID, msg.From.ID, p.SessionID, text)
			return
		}
	}
	if path := a.pinned(msg.From.ID); path != "" {
		ws, err := a.ensureWorkspace(ctx, path)
		if err != nil {
			a.send(msg.Chat.ID, "Pinned workspace is unavailable: "+err.Error(), nil)
			return
		}
		a.startNewTask(ctx, msg.Chat.ID, msg.From.ID, ws.ID, text)
		return
	}
	a.send(msg.Chat.ID, "Choose a workspace first with /new or /workspace.", nil)
}

func (a *App) handleCallback(ctx context.Context, q *tgCallbackQuery) {
	if q.From == nil || !a.allowed(q.From.ID) {
		return
	}
	a.answerCallback(q.ID, "")
	data := q.Data
	chatID := q.Message.Chat.ID
	switch {
	case data == "model:scopes":
		a.clearPending(q.From.ID)
		a.editMessageText(chatID, q.Message.MessageID, modelScopeText(false), modelScopeKeyboard())
	case data == "model:cancel":
		a.clearPending(q.From.ID)
		a.editMessageText(chatID, q.Message.MessageID, "Model selection cancelled.", nil)
	case data == "model:refresh":
		a.refreshModels(ctx, chatID, q.Message.MessageID)
	case strings.HasPrefix(data, "mscope:"):
		a.chooseModelScope(ctx, chatID, q.Message.MessageID, q.From.ID, strings.TrimPrefix(data, "mscope:"))
	case strings.HasPrefix(data, "mprov:"):
		a.chooseModelProvider(ctx, chatID, q.Message.MessageID, q.From.ID, strings.TrimPrefix(data, "mprov:"))
	case strings.HasPrefix(data, "mmodel:"):
		index, _ := strconv.Atoi(strings.TrimPrefix(data, "mmodel:"))
		a.chooseModel(ctx, chatID, q.Message.MessageID, q.From.ID, index)
	case strings.HasPrefix(data, "mwp:"):
		page, _ := strconv.Atoi(strings.TrimPrefix(data, "mwp:"))
		a.editWorkspaces(ctx, chatID, q.Message.MessageID, "Choose workspace:", "mws:", page)
	case strings.HasPrefix(data, "mswp:"):
		page, _ := strconv.Atoi(strings.TrimPrefix(data, "mswp:"))
		a.editWorkspaces(ctx, chatID, q.Message.MessageID, "Choose workspace:", "msws:", page)
	case strings.HasPrefix(data, "mws:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "mws:"), 10, 64)
		a.applyWorkspaceModel(ctx, chatID, q.Message.MessageID, q.From.ID, id)
	case strings.HasPrefix(data, "msws:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "msws:"), 10, 64)
		a.chooseSessionWorkspace(ctx, chatID, q.Message.MessageID, q.From.ID, id, 0)
	case strings.HasPrefix(data, "msp:"):
		parts := strings.Split(strings.TrimPrefix(data, "msp:"), ":")
		if len(parts) == 2 {
			id, _ := strconv.ParseInt(parts[0], 10, 64)
			page, _ := strconv.Atoi(parts[1])
			a.chooseSessionWorkspace(ctx, chatID, q.Message.MessageID, q.From.ID, id, page)
		}
	case strings.HasPrefix(data, "msess:"):
		a.applySessionModel(ctx, chatID, q.Message.MessageID, q.From.ID, strings.TrimPrefix(data, "msess:"))
	case strings.HasPrefix(data, "wp:"):
		page, _ := strconv.Atoi(strings.TrimPrefix(data, "wp:"))
		a.editWorkspaces(ctx, chatID, q.Message.MessageID, "Choose a workspace:", "w:", page)
	case strings.HasPrefix(data, "newwsp:"):
		page, _ := strconv.Atoi(strings.TrimPrefix(data, "newwsp:"))
		a.editWorkspaces(ctx, chatID, q.Message.MessageID, "Choose a workspace:", "newws:", page)
	case strings.HasPrefix(data, "w:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "w:"), 10, 64)
		a.editSessions(ctx, chatID, q.Message.MessageID, id, 0)
	case strings.HasPrefix(data, "newws:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "newws:"), 10, 64)
		p, _ := a.getPending(q.From.ID)
		if p.Prompt != "" {
			a.clearPending(q.From.ID)
			a.startNewTask(ctx, chatID, q.From.ID, id, p.Prompt)
			return
		}
		a.setPending(q.From.ID, pending{Kind: "await_new_prompt", WorkspaceID: id})
		a.send(chatID, "Send the task description.", nil)
	case strings.HasPrefix(data, "ns:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "ns:"), 10, 64)
		a.setPending(q.From.ID, pending{Kind: "await_new_prompt", WorkspaceID: id})
		a.send(chatID, "Send the task description.", nil)
	case strings.HasPrefix(data, "s:"):
		sid := strings.TrimPrefix(data, "s:")
		a.setPending(q.From.ID, pending{Kind: "await_resume_prompt", SessionID: sid})
		a.send(chatID, "Send the message to continue this session.", nil)
	case strings.HasPrefix(data, "sp:"):
		parts := strings.Split(strings.TrimPrefix(data, "sp:"), ":")
		if len(parts) == 2 {
			id, _ := strconv.ParseInt(parts[0], 10, 64)
			page, _ := strconv.Atoi(parts[1])
			a.editSessions(ctx, chatID, q.Message.MessageID, id, page)
		}
	case strings.HasPrefix(data, "sessions:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "sessions:"), 10, 64)
		a.sendSessions(ctx, chatID, id, 0)
	case strings.HasPrefix(data, "new:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "new:"), 10, 64)
		a.setPending(q.From.ID, pending{Kind: "await_new_prompt", WorkspaceID: id})
		a.send(chatID, "Send the task description.", nil)
	case strings.HasPrefix(data, "pin:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "pin:"), 10, 64)
		ws, err := a.store.GetWorkspace(ctx, id)
		if err != nil {
			a.send(chatID, "Pin failed: "+err.Error(), nil)
			return
		}
		a.mu.Lock()
		a.pins[q.From.ID] = ws.Path
		a.mu.Unlock()
		a.send(chatID, fmt.Sprintf("📌 Workspace pinned: %s\nNew private messages will create tasks in this workspace.\nUse /unpin or /workspace to change it.", ws.Path), nil)
	}
}

func (a *App) handleTopicMessage(ctx context.Context, msg *tgMessage) {
	sess, err := a.store.GetSessionByTopic(ctx, msg.MessageThreadID)
	if err != nil {
		return
	}
	ws, err := a.store.GetWorkspace(ctx, sess.WorkspaceID)
	if err != nil {
		a.replyTopic(msg, "Could not find the workspace for this session.")
		return
	}
	req := runner.StartRequest{
		SessionID: sess.ID, Title: displaySession(sess), Workspace: ws.Path, TopicID: sess.TopicID,
		Model: a.resolveModel(sess, ws), Existing: true,
	}
	if err := a.runner.Steer(ctx, req, msg.Text); err != nil {
		a.replyTopic(msg, "Failed to send to pi: "+err.Error())
	}
}

func (a *App) syncSessions(ctx context.Context, chatID int64) {
	items, err := session.Scan(ctx, a.cfg.Runner.SessionDir)
	if err != nil {
		a.send(chatID, "Sync failed: "+err.Error(), nil)
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
	a.send(chatID, fmt.Sprintf("Sync complete: found %d new workspaces and %d sessions.", newWS, newSess), nil)
}

func (a *App) sendModelScopes(ctx context.Context, chatID, userID int64, args string) {
	refresh := args == "--refresh"
	if args != "" && !refresh {
		a.send(chatID, "Usage: /model or /model --refresh", nil)
		return
	}
	a.clearPending(userID)
	if refresh {
		if _, err := a.runner.AvailableModels(ctx, true); err != nil {
			a.send(chatID, "Failed to refresh models from pi: "+err.Error(), nil)
			return
		}
	}
	a.send(chatID, modelScopeText(refresh), modelScopeKeyboard())
}

func (a *App) refreshModels(ctx context.Context, chatID int64, messageID int) {
	if _, err := a.runner.AvailableModels(ctx, true); err != nil {
		a.editMessageText(chatID, messageID, "Failed to refresh models from pi: "+err.Error(), nil)
		return
	}
	a.editMessageText(chatID, messageID, modelScopeText(true), modelScopeKeyboard())
}

func (a *App) chooseModelScope(ctx context.Context, chatID int64, messageID int, userID int64, scope string) {
	if scope != "global" && scope != "workspace" && scope != "session" {
		a.editMessageText(chatID, messageID, "Unknown model scope.", nil)
		return
	}
	a.setPending(userID, pending{Kind: "model", ModelScope: scope})
	a.editModelProviders(ctx, chatID, messageID, scope)
}

func (a *App) editModelProviders(ctx context.Context, chatID int64, messageID int, scope string) {
	models, err := a.runner.AvailableModels(ctx, false)
	if err != nil {
		a.editMessageText(chatID, messageID, "Failed to read models from pi: "+err.Error(), nil)
		return
	}
	providers := modelProviders(models)
	if len(providers) == 0 {
		a.editMessageText(chatID, messageID, "pi returned no available model providers.", nil)
		return
	}
	var rows [][]inlineKeyboardButton
	for i := 0; i < len(providers); i += 2 {
		var row []inlineKeyboardButton
		for _, provider := range providers[i:min(i+2, len(providers))] {
			row = append(row, inlineKeyboardButton{Text: provider, CallbackData: "mprov:" + provider})
		}
		rows = append(rows, row)
	}
	rows = append(rows, inlineKeyboardRow(
		inlineKeyboardButton{Text: "< Scope", CallbackData: "model:scopes"},
		inlineKeyboardButton{Text: "Cancel", CallbackData: "model:cancel"},
	))
	a.editMessageText(chatID, messageID, "Choose provider for "+scopeLabel(scope)+":", inlineKeyboardMarkup{InlineKeyboard: rows})
}

func (a *App) chooseModelProvider(ctx context.Context, chatID int64, messageID int, userID int64, provider string) {
	p, ok := a.getPending(userID)
	if !ok || p.Kind != "model" || p.ModelScope == "" {
		a.editMessageText(chatID, messageID, "Model selection expired. Send /model again.", nil)
		return
	}
	p.Provider = provider
	a.setPending(userID, p)
	a.editProviderModels(ctx, chatID, messageID, p.ModelScope, provider)
}

func (a *App) editProviderModels(ctx context.Context, chatID int64, messageID int, scope, provider string) {
	models, err := a.runner.AvailableModels(ctx, false)
	if err != nil {
		a.editMessageText(chatID, messageID, "Failed to read models from pi: "+err.Error(), nil)
		return
	}
	models = filterProviderModels(models, provider)
	if len(models) == 0 {
		a.editMessageText(chatID, messageID, "No models found for "+provider+".", nil)
		return
	}
	var rows [][]inlineKeyboardButton
	widths := modelButtonWidths(models)
	for i, model := range models {
		rows = append(rows, inlineKeyboardRow(inlineKeyboardButton{
			Text:         modelButtonText(model, widths),
			CallbackData: "mmodel:" + strconv.Itoa(i),
		}))
	}
	rows = append(rows, inlineKeyboardRow(
		inlineKeyboardButton{Text: "< Provider", CallbackData: "mscope:" + scope},
		inlineKeyboardButton{Text: "Cancel", CallbackData: "model:cancel"},
	))
	a.editMessageText(chatID, messageID, "Choose model from "+provider+":", inlineKeyboardMarkup{InlineKeyboard: rows})
}

func (a *App) chooseModel(ctx context.Context, chatID int64, messageID int, userID int64, index int) {
	p, ok := a.getPending(userID)
	if !ok || p.Kind != "model" || p.ModelScope == "" || p.Provider == "" {
		a.editMessageText(chatID, messageID, "Model selection expired. Send /model again.", nil)
		return
	}
	models, err := a.runner.AvailableModels(ctx, false)
	if err != nil {
		a.editMessageText(chatID, messageID, "Failed to read models from pi: "+err.Error(), nil)
		return
	}
	models = filterProviderModels(models, p.Provider)
	if index < 0 || index >= len(models) {
		a.editMessageText(chatID, messageID, "Model selection expired. Send /model again.", nil)
		return
	}
	model := models[index]
	p.ModelID = model.Provider + "/" + model.ID
	a.setPending(userID, p)
	switch p.ModelScope {
	case "global":
		a.applyGlobalModel(chatID, messageID, userID, p.ModelID)
	case "workspace":
		a.editWorkspaces(ctx, chatID, messageID, "Choose workspace for "+modelDisplay(model)+":", "mws:", 0)
	case "session":
		a.editWorkspaces(ctx, chatID, messageID, "Choose workspace:", "msws:", 0)
	}
}

func (a *App) applyGlobalModel(chatID int64, messageID int, userID int64, model string) {
	if err := config.SetGlobalModel(a.paths.ConfigPath, model); err != nil {
		a.editMessageText(chatID, messageID, "Failed to update config: "+err.Error(), nil)
		return
	}
	a.cfg.GlobalModel = model
	a.clearPending(userID)
	a.editMessageText(chatID, messageID, "Global model set to "+model+".", nil)
}

func (a *App) applyWorkspaceModel(ctx context.Context, chatID int64, messageID int, userID int64, workspaceID int64) {
	p, ok := a.getPending(userID)
	if !ok || p.Kind != "model" || p.ModelScope != "workspace" || p.ModelID == "" {
		a.editMessageText(chatID, messageID, "Model selection expired. Send /model again.", nil)
		return
	}
	ws, err := a.store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		a.editMessageText(chatID, messageID, "Failed to read workspace: "+err.Error(), nil)
		return
	}
	if err := a.store.SetWorkspaceModel(ctx, workspaceID, p.ModelID); err != nil {
		a.editMessageText(chatID, messageID, "Failed to update workspace model: "+err.Error(), nil)
		return
	}
	a.clearPending(userID)
	a.editMessageText(chatID, messageID, fmt.Sprintf("Workspace model set to %s for %s.", p.ModelID, workspaceLabel(ws)), nil)
}

func (a *App) chooseSessionWorkspace(ctx context.Context, chatID int64, messageID int, userID int64, workspaceID int64, page int) {
	p, ok := a.getPending(userID)
	if !ok || p.Kind != "model" || p.ModelScope != "session" || p.ModelID == "" {
		a.editMessageText(chatID, messageID, "Model selection expired. Send /model again.", nil)
		return
	}
	p.WorkspaceID = workspaceID
	a.setPending(userID, p)
	a.editModelSessions(ctx, chatID, messageID, workspaceID, page)
}

func (a *App) editModelSessions(ctx context.Context, chatID int64, messageID int, workspaceID int64, page int) {
	total, err := a.store.CountSessions(ctx, workspaceID)
	if err != nil {
		a.editMessageText(chatID, messageID, "Failed to read sessions: "+err.Error(), nil)
		return
	}
	page = clampPage(page, total, sessionPageSize)
	sessions, err := a.store.ListSessions(ctx, workspaceID, sessionPageSize, page*sessionPageSize)
	if err != nil {
		a.editMessageText(chatID, messageID, "Failed to read sessions: "+err.Error(), nil)
		return
	}
	var rows [][]inlineKeyboardButton
	for _, sess := range sessions {
		rows = append(rows, inlineKeyboardRow(inlineKeyboardButton{Text: displaySession(sess), CallbackData: "msess:" + sess.ID}))
	}
	rows = appendPageNav(rows, page, total, sessionPageSize, "msp:"+strconv.FormatInt(workspaceID, 10)+":")
	rows = append(rows, inlineKeyboardRow(inlineKeyboardButton{Text: "Cancel", CallbackData: "model:cancel"}))
	a.editMessageText(chatID, messageID, "Choose session:", inlineKeyboardMarkup{InlineKeyboard: rows})
}

func (a *App) applySessionModel(ctx context.Context, chatID int64, messageID int, userID int64, sessionID string) {
	p, ok := a.getPending(userID)
	if !ok || p.Kind != "model" || p.ModelScope != "session" || p.ModelID == "" {
		a.editMessageText(chatID, messageID, "Model selection expired. Send /model again.", nil)
		return
	}
	sess, err := a.store.GetSession(ctx, sessionID)
	if err != nil {
		a.editMessageText(chatID, messageID, "Failed to read session: "+err.Error(), nil)
		return
	}
	if err := a.store.SetSessionModel(ctx, sessionID, p.ModelID); err != nil {
		a.editMessageText(chatID, messageID, "Failed to update session model: "+err.Error(), nil)
		return
	}
	a.clearPending(userID)
	a.editMessageText(chatID, messageID, fmt.Sprintf("Session model set to %s for %s.", p.ModelID, displaySession(sess)), nil)
}

func (a *App) sendWorkspaces(ctx context.Context, chatID int64, text, prefix string, page int) {
	a.postWorkspaces(ctx, chatID, 0, text, prefix, page)
}

func (a *App) editWorkspaces(ctx context.Context, chatID int64, messageID int, text, prefix string, page int) {
	a.postWorkspaces(ctx, chatID, messageID, text, prefix, page)
}

func (a *App) postWorkspaces(ctx context.Context, chatID int64, messageID int, text, prefix string, page int) {
	total, err := a.store.CountWorkspaces(ctx)
	if err != nil {
		a.send(chatID, "Failed to read workspaces: "+err.Error(), nil)
		return
	}
	if total == 0 {
		a.send(chatID, "No workspaces yet. Run /sync first.", nil)
		return
	}
	page = clampPage(page, total, workspacePageSize)
	workspaces, err := a.store.ListWorkspaces(ctx, workspacePageSize, page*workspacePageSize)
	if err != nil {
		a.send(chatID, "Failed to read workspaces: "+err.Error(), nil)
		return
	}
	var rows [][]inlineKeyboardButton
	for i := 0; i < len(workspaces); i += 2 {
		var buttons []inlineKeyboardButton
		for _, ws := range workspaces[i:min(i+2, len(workspaces))] {
			buttons = append(buttons, inlineKeyboardButton{Text: workspaceLabel(ws), CallbackData: prefix + strconv.FormatInt(ws.ID, 10)})
		}
		rows = append(rows, buttons)
	}
	rows = appendPageNav(rows, page, total, workspacePageSize, workspacePagePrefix(prefix))
	a.sendOrEdit(chatID, messageID, text, inlineKeyboardMarkup{InlineKeyboard: rows})
}

func (a *App) sendSessions(ctx context.Context, chatID int64, workspaceID int64, page int) {
	a.postSessions(ctx, chatID, 0, workspaceID, page)
}

func (a *App) editSessions(ctx context.Context, chatID int64, messageID int, workspaceID int64, page int) {
	a.postSessions(ctx, chatID, messageID, workspaceID, page)
}

func (a *App) postSessions(ctx context.Context, chatID int64, messageID int, workspaceID int64, page int) {
	total, err := a.store.CountSessions(ctx, workspaceID)
	if err != nil {
		a.send(chatID, "Failed to read sessions: "+err.Error(), nil)
		return
	}
	page = clampPage(page, total, sessionPageSize)
	sessions, err := a.store.ListSessions(ctx, workspaceID, sessionPageSize, page*sessionPageSize)
	if err != nil {
		a.send(chatID, "Failed to read sessions: "+err.Error(), nil)
		return
	}
	var rows [][]inlineKeyboardButton
	rows = append(rows, inlineKeyboardRow(inlineKeyboardButton{Text: "+ New Session", CallbackData: "ns:" + strconv.FormatInt(workspaceID, 10)}))
	for _, sess := range sessions {
		rows = append(rows, inlineKeyboardRow(inlineKeyboardButton{Text: displaySession(sess), CallbackData: "s:" + sess.ID}))
	}
	rows = appendPageNav(rows, page, total, sessionPageSize, "sp:"+strconv.FormatInt(workspaceID, 10)+":")
	a.sendOrEdit(chatID, messageID, "Choose a session:", inlineKeyboardMarkup{InlineKeyboard: rows})
}

func workspaceLabel(ws store.Workspace) string {
	label := ws.Name
	if label == "" {
		label = filepath.Base(ws.Path)
	}
	if len([]rune(label)) > 24 {
		label = string([]rune(label)[:24])
	}
	return label
}

func modelScopeText(refreshed bool) string {
	if refreshed {
		return "Choose model scope (models refreshed from pi):"
	}
	return "Choose model scope:"
}

func modelScopeKeyboard() inlineKeyboardMarkup {
	return inlineKeyboardMarkup{InlineKeyboard: [][]inlineKeyboardButton{
		{
			{Text: "Global", CallbackData: "mscope:global"},
			{Text: "Workspace", CallbackData: "mscope:workspace"},
			{Text: "Session", CallbackData: "mscope:session"},
		},
		{
			{Text: "Refresh", CallbackData: "model:refresh"},
			{Text: "Cancel", CallbackData: "model:cancel"},
		},
	}}
}

func scopeLabel(scope string) string {
	switch scope {
	case "global":
		return "Global"
	case "workspace":
		return "Workspace"
	case "session":
		return "Session"
	default:
		return scope
	}
}

func modelProviders(models []runner.ModelInfo) []string {
	seen := map[string]bool{}
	var providers []string
	for _, model := range models {
		if model.Provider == "" || seen[model.Provider] {
			continue
		}
		seen[model.Provider] = true
		providers = append(providers, model.Provider)
	}
	return providers
}

func filterProviderModels(models []runner.ModelInfo, provider string) []runner.ModelInfo {
	var out []runner.ModelInfo
	for _, model := range models {
		if model.Provider == provider {
			out = append(out, model)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return modelDisplay(out[i]) < modelDisplay(out[j])
	})
	return out
}

type modelButtonColumnWidths struct {
	Name float64
	Ctx  float64
	Out  float64
	Icons int
}

func visualWidth(s string) float64 {
	var w float64
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			if r == 'W' || r == 'M' {
				w += 1.4
			} else if r == 'I' {
				w += 0.5
			} else {
				w += 1.1
			}
		case r >= 'a' && r <= 'z':
			if r == 'm' || r == 'w' {
				w += 1.3
			} else if r == 'i' || r == 'l' || r == 'j' || r == 't' || r == 'f' {
				w += 0.5
			} else {
				w += 0.95
			}
		case r >= '0' && r <= '9':
			w += 1.1
		case r == ' ' || r == '-' || r == '.':
			w += 0.55
		case r == '\u2002' || r == '\u2007':
			w += 1.1
		case r == '\u3000':
			w += 2.0
		case r > 127:
			w += 2.1
		default:
			w += 1.0
		}
	}
	return w
}

func truncateVisual(s string, maxWidth float64) string {
	if visualWidth(s) <= maxWidth {
		return s
	}
	target := maxWidth - 1.0 // reserve space for "…"
	var res string
	var w float64
	for _, r := range s {
		rw := visualWidth(string(r))
		if w+rw > target {
			break
		}
		res += string(r)
		w += rw
	}
	return res + "…"
}

func padRightVisual(s string, targetWidth float64) string {
	w := visualWidth(s)
	if w >= targetWidth {
		return s
	}
	diff := targetWidth - w
	enSpaces := int(diff / 1.1)
	rem := diff - float64(enSpaces)*1.1
	regSpaces := int(rem / 0.55)
	
	res := s
	if enSpaces > 0 {
		res += strings.Repeat("\u2002", enSpaces)
	}
	if regSpaces > 0 {
		res += strings.Repeat(" ", regSpaces)
	}
	return res
}

func padLeftVisual(s string, targetWidth float64) string {
	w := visualWidth(s)
	if w >= targetWidth {
		return s
	}
	diff := targetWidth - w
	enSpaces := int(diff / 1.1)
	rem := diff - float64(enSpaces)*1.1
	regSpaces := int(rem / 0.55)
	
	pad := ""
	if enSpaces > 0 {
		pad += strings.Repeat("\u2002", enSpaces)
	}
	if regSpaces > 0 {
		pad += strings.Repeat(" ", regSpaces)
	}
	return pad + s
}

const maxNameWidthLimit = 12.5

func modelButtonWidths(models []runner.ModelInfo) modelButtonColumnWidths {
	var widths modelButtonColumnWidths
	for _, model := range models {
		if w := visualWidth(modelDisplay(model)); w > widths.Name {
			widths.Name = w
		}
		if widths.Name > maxNameWidthLimit {
			widths.Name = maxNameWidthLimit
		}
		if model.ContextWindow > 0 {
			if w := visualWidth(compactNumber(model.ContextWindow)); w > widths.Ctx {
				widths.Ctx = w
			}
		}
		if model.MaxTokens > 0 {
			if w := visualWidth(compactNumber(model.MaxTokens)); w > widths.Out {
				widths.Out = w
			}
		}
		icons := 0
		if model.Reasoning {
			icons++
		}
		for _, input := range model.Inputs {
			if input == "text" || input == "image" {
				icons++
			}
		}
		if icons > widths.Icons {
			widths.Icons = icons
		}
	}
	return widths
}

func modelButtonText(model runner.ModelInfo, widths modelButtonColumnWidths) string {
	displayName := truncateVisual(modelDisplay(model), widths.Name)
	name := padRightVisual(displayName, widths.Name)
	ctx := padLeftVisual("-", widths.Ctx)
	if model.ContextWindow > 0 {
		ctx = padLeftVisual(compactNumber(model.ContextWindow), widths.Ctx)
	}
	out := padLeftVisual("-", widths.Out)
	if model.MaxTokens > 0 {
		out = padLeftVisual(compactNumber(model.MaxTokens), widths.Out)
	}
	
	var icons []string
	if model.Reasoning {
		icons = append(icons, "💭")
	}
	for _, input := range model.Inputs {
		switch input {
		case "text":
			icons = append(icons, "📝")
		case "image":
			icons = append(icons, "🖼")
		}
	}
	
	iconStr := ""
	// 模型能力 右对齐: 补全左侧缺失的图标宽度
	padCount := widths.Icons - len(icons)
	for i := 0; i < padCount; i++ {
		iconStr += "\u3000" // U+3000 (Ideographic Space) perfectly matches Emoji width
	}
	for _, icon := range icons {
		iconStr += icon
	}
	if iconStr == "" {
		return name + " \u2002📚" + ctx + " ↗ " + out
	}
	return name + " \u2002📚" + ctx + " ↗ " + out + "\u2002" + iconStr
}


func modelDisplay(model runner.ModelInfo) string {
	if model.Name != "" {
		return model.Name
	}
	return model.ID
}

func compactNumber(v int64) string {
	if v >= 1_000_000 {
		if v%1_000_000 == 0 {
			return strconv.FormatInt(v/1_000_000, 10) + "M"
		}
		return strconv.FormatFloat(float64(v)/1_000_000, 'f', 1, 64) + "M"
	}
	if v >= 1_000 {
		return strconv.FormatFloat(float64(v)/1000, 'f', 0, 64) + "K"
	}
	return strconv.FormatInt(v, 10)
}



func appendPageNav(rows [][]inlineKeyboardButton, page, total, pageSize int, prefix string) [][]inlineKeyboardButton {
	if total <= pageSize {
		return rows
	}
	var nav []inlineKeyboardButton
	if page > 0 {
		nav = append(nav, inlineKeyboardButton{Text: "< Prev", CallbackData: prefix + strconv.Itoa(page-1)})
	}
	if (page+1)*pageSize < total {
		nav = append(nav, inlineKeyboardButton{Text: "Next >", CallbackData: prefix + strconv.Itoa(page+1)})
	}
	return append(rows, nav)
}

func clampPage(page, total, pageSize int) int {
	if page < 0 {
		return 0
	}
	pages := (total + pageSize - 1) / pageSize
	if pages == 0 || page < pages {
		return page
	}
	return pages - 1
}

func workspacePagePrefix(prefix string) string {
	if prefix == "newws:" {
		return "newwsp:"
	}
	if prefix == "mws:" {
		return "mwp:"
	}
	if prefix == "msws:" {
		return "mswp:"
	}
	return "wp:"
}

func (a *App) startNewTask(ctx context.Context, chatID, userID int64, workspaceID int64, prompt string) {
	ws, err := a.store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		a.send(chatID, "Failed to read workspace: "+err.Error(), nil)
		return
	}
	title := topicTitle(prompt)
	topicID, err := a.createForumTopic(title)
	if err != nil {
		a.send(chatID, "Failed to create topic: "+err.Error(), nil)
		return
	}
	goalID, err := a.sendTopicMessage(topicID, "🎯 "+prompt, nil)
	if err == nil {
		_ = a.pinChatMessage(goalID)
	}
	sess, err := a.store.CreatePlaceholderSession(ctx, workspaceID, title)
	if err != nil {
		a.send(chatID, "Failed to create session: "+err.Error(), nil)
		return
	}
	_ = a.store.SetSessionTopic(ctx, sess.ID, topicID, goalID)
	req := runner.StartRequest{SessionID: sess.ID, Title: title, Workspace: ws.Path, TopicID: topicID, Model: a.resolveModel(sess, ws)}
	if err := a.runner.Prompt(ctx, req, prompt); err != nil {
		a.send(chatID, "Failed to start pi: "+err.Error(), nil)
		return
	}
	a.send(chatID, fmt.Sprintf("Created topic: %s", title), taskKeyboard(workspaceID, a.pinned(userID) == ws.Path))
}

func (a *App) resumeSession(ctx context.Context, chatID, userID int64, sessionID, prompt string) {
	sess, err := a.store.GetSession(ctx, sessionID)
	if err != nil {
		a.send(chatID, "Failed to read session: "+err.Error(), nil)
		return
	}
	ws, err := a.store.GetWorkspace(ctx, sess.WorkspaceID)
	if err != nil {
		a.send(chatID, "Failed to read workspace: "+err.Error(), nil)
		return
	}
	if sess.TopicID == 0 {
		topicID, err := a.createForumTopic(topicTitle(prompt))
		if err != nil {
			a.send(chatID, "Failed to create topic: "+err.Error(), nil)
			return
		}
		goalID, _ := a.sendTopicMessage(topicID, "🎯 "+prompt, nil)
		_ = a.pinChatMessage(goalID)
		_ = a.store.SetSessionTopic(ctx, sess.ID, topicID, goalID)
		sess.TopicID = topicID
	}
	req := runner.StartRequest{SessionID: sess.ID, Title: displaySession(sess), Workspace: ws.Path, TopicID: sess.TopicID, Model: a.resolveModel(sess, ws), Existing: true}
	if err := a.runner.Prompt(ctx, req, prompt); err != nil {
		a.send(chatID, "Failed to start pi: "+err.Error(), nil)
		return
	}
	a.send(chatID, "Sent to session.", taskKeyboard(ws.ID, a.pinned(userID) == ws.Path))
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

func (a *App) sendOrEdit(chatID int64, messageID int, text string, replyMarkup any) {
	if messageID == 0 {
		a.send(chatID, text, replyMarkup)
		return
	}
	if err := a.editMessageText(chatID, messageID, text, replyMarkup); err != nil {
		a.log.Warn("telegram edit failed", "error", err)
	}
}

func (a *App) replyTopic(msg *tgMessage, text string) {
	_, _ = a.sendMessage(msg.Chat.ID, msg.MessageThreadID, text, nil)
}

func (a *App) answerCallback(id, text string) {
	var resp telegramOK
	_ = a.callTelegram("answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, &resp)
}

func (a *App) registerCommands(ctx context.Context) error {
	var resp telegramOK
	err := a.callTelegramCtx(ctx, "setMyCommands", map[string]any{
		"commands": []botCommand{
			{Command: "start", Description: "Show help"},
			{Command: "workspace", Description: "Choose a workspace and session"},
			{Command: "new", Description: "Start a new task"},
			{Command: "sync", Description: "Import historical sessions"},
			{Command: "unpin", Description: "Clear the pinned workspace"},
			{Command: "model", Description: "Configure model settings"},
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

func (a *App) editMessageText(chatID int64, messageID int, text string, replyMarkup any) error {
	var resp telegramOK
	payload := map[string]any{"chat_id": chatID, "message_id": messageID, "text": text}
	if replyMarkup != nil {
		payload["reply_markup"] = replyMarkup
	}
	if err := a.callTelegram("editMessageText", payload, &resp); err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Description)
	}
	return nil
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

type botCommand struct {
	Command     string `json:"command"`
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
