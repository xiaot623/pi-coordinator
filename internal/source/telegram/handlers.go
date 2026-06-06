package telegram

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xiaot/pi-coordinator/internal/runner"
	"github.com/xiaot/pi-coordinator/internal/store"
)

// registerHandlers sets up all routes for the Telegram bot.
func (b *Bot) registerHandlers() {
	// Commands
	b.router.Command("help", handleHelp)
	b.router.Command("sync", handleSync)
	b.router.Command("workspace", handleWorkspaceCmd)
	b.router.Command("new", handleNewCmd)
	b.router.Command("pin", handlePinCmd)
	b.router.Command("unpin", handleUnpin)
	b.router.Command("model", handleModelCmd)
	b.router.Command("bots", handleBotsCmd)

	// Generic text messages
	b.router.Text(handlePrivateMessage)
	b.router.Topic(handleTopicMessage)

	// Callbacks
	b.router.Callback("model:scopes", handleModelScopes)
	b.router.Callback("model:cancel", handleModelCancel)
	b.router.Callback("model:refresh", handleModelRefresh)
	b.router.Callback("mscope:", handleChooseModelScope)
	b.router.Callback("mprov:", handleChooseModelProvider)
	b.router.Callback("mmodel:", handleChooseModel)
	b.router.Callback("mwp:", handleModelWorkspacePage)
	b.router.Callback("mswp:", handleModelSessionWorkspacePage)
	b.router.Callback("mws:", handleApplyWorkspaceModel)
	b.router.Callback("msws:", handleChooseSessionWorkspace)
	b.router.Callback("msp:", handleModelSessionPage)
	b.router.Callback("msess:", handleApplySessionModel)

	b.router.Callback("wp:", handleWorkspacePage)
	b.router.Callback("newwsp:", handleNewWorkspacePage)
	b.router.Callback("pinwsp:", handlePinWorkspacePage)
	b.router.Callback("w:", handleChooseWorkspaceForSession)
	b.router.Callback("newws:", handleChooseWorkspaceForNewTask)
	b.router.Callback("pinws:", handleChooseWorkspaceForPin)

	b.router.Callback("ns:", handleNewSessionCallback)
	b.router.Callback("s:", handleSessionCallback)
	b.router.Callback("sp:", handleSessionPage)
	b.router.Callback("sessions:", handleSessionsList)
	b.router.Callback("new:", handleNewCallback)
	b.router.Callback("pin:", handlePinCallback)
	b.router.Callback("trace:retry", handleTraceRetry)
}

// -- Command Handlers --

func handleHelp(ctx context.Context, b *Bot, update Update) {
	b.send(update.Message.Chat.ID, "pico is ready. Use /sync to import history, or /new to start a task.", nil)
}

func handleSync(ctx context.Context, b *Bot, update Update) {
	newWS, newSess, err := b.app.SyncSessions(ctx)
	if err != nil {
		b.send(update.Message.Chat.ID, "Sync failed: "+err.Error(), nil)
		return
	}
	b.send(update.Message.Chat.ID, fmt.Sprintf("Sync complete: found %d new workspaces and %d sessions.", newWS, newSess), nil)
}

func handleWorkspaceCmd(ctx context.Context, b *Bot, update Update) {
	b.clearPending(update.Message.From.ID)
	sendWorkspaces(ctx, b, update.Message.Chat.ID, 0, "Choose a workspace:", "w:", 0)
}

func handleNewCmd(ctx context.Context, b *Bot, update Update) {
	prompt := strings.TrimSpace(update.Message.CommandArguments())
	if path := b.pinned(update.Message.From.ID); path != "" {
		ws, err := b.app.Store().GetWorkspaceByPath(ctx, path)
		if err != nil {
			b.send(update.Message.Chat.ID, "Pinned workspace is unavailable: "+err.Error(), nil)
			return
		}
		if prompt != "" {
			startNewTask(ctx, b, update.Message.Chat, update.Message.From.ID, ws.ID, prompt, nil)
			return
		}
		promptForPinnedNewTask(b, update.Message.Chat, ws.Path)
		return
	}
	b.setPending(update.Message.From.ID, PendingState{Kind: "new_prompt", Prompt: prompt})
	sendWorkspaces(ctx, b, update.Message.Chat.ID, 0, "Choose a workspace:", "newws:", 0)
}

func handlePinCmd(ctx context.Context, b *Bot, update Update) {
	b.clearPending(update.Message.From.ID)
	args := strings.TrimSpace(update.Message.CommandArguments())
	if args == "" {
		sendWorkspaces(ctx, b, update.Message.Chat.ID, 0, "Choose a workspace to pin:", "pinws:", 0)
		return
	}
	ws, err := findWorkspaceForPin(ctx, b, args)
	if err != nil {
		b.send(update.Message.Chat.ID, "Pin failed: "+err.Error(), nil)
		return
	}
	pinWorkspace(ctx, b, update.Message.From.ID, update.Message.Chat.ID, update.Message.MessageThreadID, ws)
}

func handleUnpin(ctx context.Context, b *Bot, update Update) {
	b.clearUserPin(ctx, update.Message.From.ID)
	b.send(update.Message.Chat.ID, "Workspace pin cleared.", nil)
}

func handleModelCmd(ctx context.Context, b *Bot, update Update) {
	args := strings.TrimSpace(update.Message.CommandArguments())
	refresh := args == "--refresh"
	if args != "" && !refresh {
		b.send(update.Message.Chat.ID, "Usage: /model or /model --refresh", nil)
		return
	}
	b.clearPending(update.Message.From.ID)
	if refresh {
		if _, err := b.app.Runner().AvailableModels(ctx, true); err != nil {
			b.send(update.Message.Chat.ID, "Failed to refresh models from pi: "+err.Error(), nil)
			return
		}
	}
	b.send(update.Message.Chat.ID, modelScopeText(refresh), modelScopeKeyboard())
}

func handleBotsCmd(ctx context.Context, b *Bot, update Update) {
	b.listManagedBots(update.Message.Chat.ID)
}

// -- Message Handlers --

func handlePrivateMessage(ctx context.Context, b *Bot, update Update) {
	msg := update.Message
	text := effectiveText(msg)
	images, err := b.extractImages(ctx, msg)
	if err != nil {
		b.app.Logger().Warn("telegram extract images failed", "error", err)
	}
	userID := msg.From.ID
	chatID := msg.Chat.ID

	if p, ok := b.getPending(userID); ok {
		switch p.Kind {
		case "await_new_prompt":
			b.clearPending(userID)
			startNewTask(ctx, b, msg.Chat, userID, p.WorkspaceID, text, images)
			return
		case "await_resume_prompt":
			b.clearPending(userID)
			resumeSession(ctx, b, msg.Chat, userID, p.SessionID, text, images)
			return
		}
	}

	if path := b.pinned(userID); path != "" {
		ws, err := b.app.Store().GetWorkspaceByPath(ctx, path)
		if err != nil {
			b.send(chatID, "Pinned workspace is unavailable: "+err.Error(), nil)
			return
		}
		startNewTask(ctx, b, msg.Chat, userID, ws.ID, text, images)
		return
	}
	if !msg.Chat.IsPrivate() {
		return
	}
	b.send(chatID, "Choose a workspace first with /new or /workspace.", nil)
}

func promptForPendingInput(b *Bot, chat Chat, userID int64, pending PendingState, text string) {
	if !chat.IsPrivate() {
		if b.pinned(userID) != "" {
			text += "\n\nYou can send it as a normal message in General Topic."
		} else {
			text += "\n\nReply to this message with your content."
		}
	}
	messageID, err := b.sendMessage(chat.ID, 0, text, nil)
	if err != nil {
		b.app.Logger().Warn("telegram send failed", "error", err)
		return
	}
	pending.PromptChatID = chat.ID
	pending.PromptMessageID = messageID
	b.setPending(userID, pending)
}

func promptForPinnedNewTask(b *Bot, chat Chat, workspacePath string) {
	text := "Send the task description.\nPinned workspace: " + workspacePath
	if !chat.IsPrivate() {
		text += "\n\nYou can send it as a normal message in General Topic."
	}
	b.send(chat.ID, text, nil)
}

func handleTopicMessage(ctx context.Context, b *Bot, update Update) {
	msg := update.Message
	text := effectiveText(msg)
	images, err := b.extractImages(ctx, msg)
	if err != nil {
		b.app.Logger().Warn("telegram extract images failed", "error", err)
	}
	sess, err := b.app.Store().GetSessionByTopic(ctx, msg.MessageThreadID)
	if err != nil {
		return
	}
	ws, err := b.app.Store().GetWorkspace(ctx, sess.WorkspaceID)
	if err != nil {
		b.sendMessage(msg.Chat.ID, msg.MessageThreadID, "Could not find the workspace for this session.", nil)
		return
	}

	req := runner.StartRequest{
		SessionID: sess.ID,
		Title:     displaySession(sess),
		Workspace: ws.Path,
		TopicID:   sess.TopicID,
		Model:     b.app.ResolveModel(sess, ws),
		Existing:  true,
		Role:      genericRole,
	}
	rememberTraceRetry(b, msg.From.ID, "retry_resume_task", msg.Chat, 0, sess.ID, text, images)
	if traceBot, err := b.ensureTraceBot(ctx, req.Role, msg.Chat.ID); err != nil {
		return
	} else {
		req.TraceTelegramToken = traceBot.Token
		req.TraceTelegramChatIDs = traceBot.ChatIDs
	}

	runnerPrompt := appendImageContext(text, images)
	if err := b.app.Runner().Steer(ctx, req, runnerPrompt, images); err != nil {
		b.sendMessage(msg.Chat.ID, msg.MessageThreadID, "Failed to send to pi: "+err.Error(), nil)
		return
	}
	clearTraceRetry(b, msg.From.ID)
}

// -- Callbacks (Model related) --

func handleModelScopes(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	b.clearPending(q.From.ID)
	b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, modelScopeText(false), modelScopeKeyboard())
}

func handleModelCancel(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	b.clearPending(q.From.ID)
	b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Model selection cancelled.", nil)
}

func handleModelRefresh(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	if _, err := b.app.Runner().AvailableModels(ctx, true); err != nil {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Failed to refresh models from pi: "+err.Error(), nil)
		return
	}
	b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, modelScopeText(true), modelScopeKeyboard())
}

func handleChooseModelScope(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	scope := strings.TrimPrefix(q.Data, "mscope:")
	if scope != "global" && scope != "workspace" && scope != "session" {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Unknown model scope.", nil)
		return
	}
	b.setPending(q.From.ID, PendingState{Kind: "model", ModelScope: scope})
	editModelProviders(ctx, b, q.Message.Chat.ID, q.Message.MessageID, scope)
}

func handleTraceRetry(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	p, ok := b.getPending(q.From.ID)
	if !ok {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "No pending trace setup to retry.", nil)
		return
	}
	chat := Chat{ID: p.TaskChatID, Type: p.TaskChatType}
	if chat.ID == 0 {
		chat = q.Message.Chat
	}
	images := decodeImagesJSON(p.ImagesJSON)
	switch p.Kind {
	case "retry_new_task":
		startNewTask(ctx, b, chat, q.From.ID, p.WorkspaceID, p.Prompt, images)
	case "retry_resume_task":
		resumeSession(ctx, b, chat, q.From.ID, p.SessionID, p.Prompt, images)
	default:
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "No pending trace setup to retry.", nil)
	}
}

func rememberTraceRetry(b *Bot, userID int64, kind string, chat Chat, workspaceID int64, sessionID, prompt string, images []runner.ImageAttachment) {
	b.setPending(userID, PendingState{
		Kind:         kind,
		WorkspaceID:  workspaceID,
		SessionID:    sessionID,
		Prompt:       prompt,
		ImagesJSON:   encodeImagesJSON(images),
		TaskChatID:   chat.ID,
		TaskChatType: chat.Type,
	})
}

func clearTraceRetry(b *Bot, userID int64) {
	if p, ok := b.getPending(userID); ok && (p.Kind == "retry_new_task" || p.Kind == "retry_resume_task") {
		b.clearPending(userID)
	}
}

func handleChooseModelProvider(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	provider := strings.TrimPrefix(q.Data, "mprov:")
	p, ok := b.getPending(q.From.ID)
	if !ok || p.Kind != "model" || p.ModelScope == "" {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Model selection expired. Send /model again.", nil)
		return
	}
	p.Provider = provider
	b.setPending(q.From.ID, p)
	editProviderModels(ctx, b, q.Message.Chat.ID, q.Message.MessageID, p.ModelScope, provider)
}

func handleChooseModel(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	index, _ := strconv.Atoi(strings.TrimPrefix(q.Data, "mmodel:"))
	p, ok := b.getPending(q.From.ID)
	if !ok || p.Kind != "model" || p.ModelScope == "" || p.Provider == "" {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Model selection expired. Send /model again.", nil)
		return
	}
	models, err := b.app.Runner().AvailableModels(ctx, false)
	if err != nil {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Failed to read models from pi: "+err.Error(), nil)
		return
	}
	models = filterProviderModels(models, p.Provider)
	if index < 0 || index >= len(models) {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Model selection expired. Send /model again.", nil)
		return
	}
	model := models[index]
	p.ModelID = model.Provider + "/" + model.ID
	b.setPending(q.From.ID, p)

	switch p.ModelScope {
	case "global":
		if err := b.app.SetGlobalModel(p.ModelID); err != nil {
			b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Failed to update config: "+err.Error(), nil)
			return
		}
		b.clearPending(q.From.ID)
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Global model set to "+p.ModelID+".", nil)
	case "workspace":
		sendWorkspaces(ctx, b, q.Message.Chat.ID, q.Message.MessageID, "Choose workspace for "+modelDisplay(model)+":", "mws:", 0)
	case "session":
		sendWorkspaces(ctx, b, q.Message.Chat.ID, q.Message.MessageID, "Choose workspace:", "msws:", 0)
	}
}

func handleModelWorkspacePage(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	page, _ := strconv.Atoi(strings.TrimPrefix(q.Data, "mwp:"))
	sendWorkspaces(ctx, b, q.Message.Chat.ID, q.Message.MessageID, "Choose workspace:", "mws:", page)
}

func handleModelSessionWorkspacePage(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	page, _ := strconv.Atoi(strings.TrimPrefix(q.Data, "mswp:"))
	sendWorkspaces(ctx, b, q.Message.Chat.ID, q.Message.MessageID, "Choose workspace:", "msws:", page)
}

func handleApplyWorkspaceModel(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	id, _ := strconv.ParseInt(strings.TrimPrefix(q.Data, "mws:"), 10, 64)

	p, ok := b.getPending(q.From.ID)
	if !ok || p.Kind != "model" || p.ModelScope != "workspace" || p.ModelID == "" {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Model selection expired. Send /model again.", nil)
		return
	}
	ws, err := b.app.Store().GetWorkspace(ctx, id)
	if err != nil {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Failed to read workspace: "+err.Error(), nil)
		return
	}
	if err := b.app.Store().SetWorkspaceModel(ctx, id, p.ModelID); err != nil {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Failed to update workspace model: "+err.Error(), nil)
		return
	}
	b.clearPending(q.From.ID)
	b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, fmt.Sprintf("Workspace model set to %s for %s.", p.ModelID, workspaceLabel(ws)), nil)
}

func handleChooseSessionWorkspace(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	workspaceID, _ := strconv.ParseInt(strings.TrimPrefix(q.Data, "msws:"), 10, 64)

	p, ok := b.getPending(q.From.ID)
	if !ok || p.Kind != "model" || p.ModelScope != "session" || p.ModelID == "" {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Model selection expired. Send /model again.", nil)
		return
	}
	p.WorkspaceID = workspaceID
	b.setPending(q.From.ID, p)
	editModelSessions(ctx, b, q.Message.Chat.ID, q.Message.MessageID, workspaceID, 0)
}

func handleModelSessionPage(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	parts := strings.Split(strings.TrimPrefix(q.Data, "msp:"), ":")
	if len(parts) == 2 {
		id, _ := strconv.ParseInt(parts[0], 10, 64)
		page, _ := strconv.Atoi(parts[1])

		p, ok := b.getPending(q.From.ID)
		if !ok || p.Kind != "model" || p.ModelScope != "session" || p.ModelID == "" {
			b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Model selection expired. Send /model again.", nil)
			return
		}
		p.WorkspaceID = id
		b.setPending(q.From.ID, p)
		editModelSessions(ctx, b, q.Message.Chat.ID, q.Message.MessageID, id, page)
	}
}

func handleApplySessionModel(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	sessionID := strings.TrimPrefix(q.Data, "msess:")

	p, ok := b.getPending(q.From.ID)
	if !ok || p.Kind != "model" || p.ModelScope != "session" || p.ModelID == "" {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Model selection expired. Send /model again.", nil)
		return
	}
	sess, err := b.app.Store().GetSession(ctx, sessionID)
	if err != nil {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Failed to read session: "+err.Error(), nil)
		return
	}
	if err := b.app.Store().SetSessionModel(ctx, sessionID, p.ModelID); err != nil {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Failed to update session model: "+err.Error(), nil)
		return
	}
	b.clearPending(q.From.ID)
	b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, fmt.Sprintf("Session model set to %s for %s.", p.ModelID, displaySession(sess)), nil)
}

// -- Callbacks (Workspace & Sessions) --

func handleWorkspacePage(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	page, _ := strconv.Atoi(strings.TrimPrefix(q.Data, "wp:"))
	sendWorkspaces(ctx, b, q.Message.Chat.ID, q.Message.MessageID, "Choose a workspace:", "w:", page)
}

func handleNewWorkspacePage(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	page, _ := strconv.Atoi(strings.TrimPrefix(q.Data, "newwsp:"))
	sendWorkspaces(ctx, b, q.Message.Chat.ID, q.Message.MessageID, "Choose a workspace:", "newws:", page)
}

func handlePinWorkspacePage(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	page, _ := strconv.Atoi(strings.TrimPrefix(q.Data, "pinwsp:"))
	sendWorkspaces(ctx, b, q.Message.Chat.ID, q.Message.MessageID, "Choose a workspace to pin:", "pinws:", page)
}

func handleChooseWorkspaceForSession(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	id, _ := strconv.ParseInt(strings.TrimPrefix(q.Data, "w:"), 10, 64)
	sendSessions(ctx, b, q.Message.Chat.ID, q.Message.MessageID, id, 0)
}

func handleChooseWorkspaceForNewTask(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	id, _ := strconv.ParseInt(strings.TrimPrefix(q.Data, "newws:"), 10, 64)
	p, _ := b.getPending(q.From.ID)
	if p.Prompt != "" {
		b.clearPending(q.From.ID)
		images := decodeImagesJSON(p.ImagesJSON)
		startNewTask(ctx, b, q.Message.Chat, q.From.ID, id, p.Prompt, images)
		return
	}
	promptForPendingInput(b, q.Message.Chat, q.From.ID, PendingState{Kind: "await_new_prompt", WorkspaceID: id}, "Send the task description.")
}

func handleChooseWorkspaceForPin(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	id, _ := strconv.ParseInt(strings.TrimPrefix(q.Data, "pinws:"), 10, 64)
	ws, err := b.app.Store().GetWorkspace(ctx, id)
	if err != nil {
		b.send(q.Message.Chat.ID, "Pin failed: "+err.Error(), nil)
		return
	}
	pinWorkspace(ctx, b, q.From.ID, q.Message.Chat.ID, q.Message.MessageThreadID, ws)
}

func handleNewSessionCallback(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	id, _ := strconv.ParseInt(strings.TrimPrefix(q.Data, "ns:"), 10, 64)
	promptForPendingInput(b, q.Message.Chat, q.From.ID, PendingState{Kind: "await_new_prompt", WorkspaceID: id}, "Send the task description.")
}

func handleSessionCallback(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	sid := strings.TrimPrefix(q.Data, "s:")
	promptForPendingInput(b, q.Message.Chat, q.From.ID, PendingState{Kind: "await_resume_prompt", SessionID: sid}, "Send the message to continue this session.")
}

func handleSessionPage(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	parts := strings.Split(strings.TrimPrefix(q.Data, "sp:"), ":")
	if len(parts) == 2 {
		id, _ := strconv.ParseInt(parts[0], 10, 64)
		page, _ := strconv.Atoi(parts[1])
		sendSessions(ctx, b, q.Message.Chat.ID, q.Message.MessageID, id, page)
	}
}

func handleSessionsList(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	id, _ := strconv.ParseInt(strings.TrimPrefix(q.Data, "sessions:"), 10, 64)
	sendSessions(ctx, b, q.Message.Chat.ID, 0, id, 0)
}

func handleNewCallback(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	id, _ := strconv.ParseInt(strings.TrimPrefix(q.Data, "new:"), 10, 64)
	promptForPendingInput(b, q.Message.Chat, q.From.ID, PendingState{Kind: "await_new_prompt", WorkspaceID: id}, "Send the task description.")
}

func handlePinCallback(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	id, _ := strconv.ParseInt(strings.TrimPrefix(q.Data, "pin:"), 10, 64)
	ws, err := b.app.Store().GetWorkspace(ctx, id)
	if err != nil {
		b.send(q.Message.Chat.ID, "Pin failed: "+err.Error(), nil)
		return
	}
	if b.pinned(q.From.ID) == ws.Path {
		b.clearUserPin(ctx, q.From.ID)
		b.send(q.Message.Chat.ID, "Workspace pin cleared.", nil)
		return
	}
	pinWorkspace(ctx, b, q.From.ID, q.Message.Chat.ID, q.Message.MessageThreadID, ws)
}

func pinWorkspace(ctx context.Context, b *Bot, userID, chatID int64, topicID int, ws store.Workspace) {
	b.unpinTrackedMessages(ctx, userID)
	b.setPin(userID, ws.Path)
	sendPinnedWorkspaceMessage(b, userID, chatID, topicID, ws)
}

func findWorkspaceForPin(ctx context.Context, b *Bot, query string) (store.Workspace, error) {
	if id, err := strconv.ParseInt(query, 10, 64); err == nil {
		if ws, err := b.app.Store().GetWorkspace(ctx, id); err == nil {
			return ws, nil
		}
	}
	if ws, err := b.app.Store().GetWorkspaceByPath(ctx, query); err == nil {
		return ws, nil
	}
	total, err := b.app.Store().CountWorkspaces(ctx)
	if err != nil {
		return store.Workspace{}, err
	}
	workspaces, err := b.app.Store().ListWorkspaces(ctx, total, 0)
	if err != nil {
		return store.Workspace{}, err
	}
	rawQuery := query
	query = strings.ToLower(rawQuery)
	var matches []store.Workspace
	for _, ws := range workspaces {
		name := strings.ToLower(ws.Name)
		base := strings.ToLower(filepath.Base(ws.Path))
		path := strings.ToLower(ws.Path)
		if query == name || query == base || strings.HasSuffix(path, query) {
			matches = append(matches, ws)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return store.Workspace{}, fmt.Errorf("multiple workspaces match %q; use /pin <workspace id>", rawQuery)
	}
	return store.Workspace{}, fmt.Errorf("workspace %q not found", rawQuery)
}

// -- Action Implementations --

func appendImageContext(prompt string, images []runner.ImageAttachment) string {
	if len(images) == 0 {
		return prompt
	}
	var sb strings.Builder
	sb.WriteString(prompt)
	if strings.TrimSpace(prompt) != "" {
		sb.WriteString("\n\n")
	}
	for i := range images {
		sb.WriteString(fmt.Sprintf("<file name=\"pasted_image_%d.jpg\">[Image provided inline by the user, no local path]</file>\n", i+1))
	}
	return strings.TrimSpace(sb.String())
}

func startNewTask(ctx context.Context, b *Bot, chat Chat, userID int64, workspaceID int64, prompt string, images []runner.ImageAttachment) {
	chatID := chat.ID
	ws, err := b.app.Store().GetWorkspace(ctx, workspaceID)
	if err != nil {
		b.send(chatID, "Failed to read workspace: "+err.Error(), nil)
		return
	}

	rememberTraceRetry(b, userID, "retry_new_task", chat, workspaceID, "", prompt, images)
	traceBot, err := b.ensureTraceBot(ctx, genericRole, chatID)
	if err != nil {
		return
	}

	title := topicTitle(prompt)
	topicID, err := b.createForumTopic(b.app.Config().Telegram.GroupChatID, title)
	if err != nil {
		b.send(chatID, "Failed to create topic: "+err.Error(), nil)
		return
	}

	goalID, err := b.sendTopicMessage(b.app.Config().Telegram.GroupChatID, topicID, "🎯 "+prompt, nil)
	if err == nil {
		_ = b.pinChatMessage(b.app.Config().Telegram.GroupChatID, goalID)
	}

	sess, err := b.app.Store().CreatePlaceholderSession(ctx, workspaceID, title)
	if err != nil {
		b.send(chatID, "Failed to create session: "+err.Error(), nil)
		return
	}
	_ = b.app.Store().SetSessionTopic(ctx, sess.ID, topicID, goalID)

	req := runner.StartRequest{
		SessionID: sess.ID,
		Title:     title,
		Workspace: ws.Path,
		TopicID:   topicID,
		Model:     b.app.ResolveModel(sess, ws),
		Role:      genericRole,
	}
	req.TraceTelegramToken = traceBot.Token
	req.TraceTelegramChatIDs = traceBot.ChatIDs

	runnerPrompt := appendImageContext(prompt, images)
	if err := b.app.Runner().Prompt(ctx, req, runnerPrompt, images); err != nil {
		b.send(chatID, "Failed to start pi: "+err.Error(), nil)
		return
	}
	clearTraceRetry(b, userID)

	b.send(chatID, fmt.Sprintf("Created topic: %s", title), createdTopicKeyboard(workspaceID, b.pinned(userID) == ws.Path, b.app.Config().Telegram.GroupChatID, topicID))
}

func resumeSession(ctx context.Context, b *Bot, chat Chat, userID int64, sessionID, prompt string, images []runner.ImageAttachment) {
	chatID := chat.ID
	sess, err := b.app.Store().GetSession(ctx, sessionID)
	if err != nil {
		b.send(chatID, "Failed to read session: "+err.Error(), nil)
		return
	}
	ws, err := b.app.Store().GetWorkspace(ctx, sess.WorkspaceID)
	if err != nil {
		b.send(chatID, "Failed to read workspace: "+err.Error(), nil)
		return
	}

	rememberTraceRetry(b, userID, "retry_resume_task", chat, 0, sessionID, prompt, images)
	traceBot, err := b.ensureTraceBot(ctx, genericRole, chatID)
	if err != nil {
		return
	}

	if sess.TopicID == 0 {
		topicID, err := b.createForumTopic(b.app.Config().Telegram.GroupChatID, topicTitle(prompt))
		if err != nil {
			b.send(chatID, "Failed to create topic: "+err.Error(), nil)
			return
		}
		goalID, _ := b.sendTopicMessage(b.app.Config().Telegram.GroupChatID, topicID, "🎯 "+prompt, nil)
		_ = b.pinChatMessage(b.app.Config().Telegram.GroupChatID, goalID)
		_ = b.app.Store().SetSessionTopic(ctx, sess.ID, topicID, goalID)
		sess.TopicID = topicID
	}

	req := runner.StartRequest{
		SessionID: sess.ID,
		Title:     displaySession(sess),
		Workspace: ws.Path,
		TopicID:   sess.TopicID,
		Model:     b.app.ResolveModel(sess, ws),
		Existing:  true,
		Role:      genericRole,
	}
	req.TraceTelegramToken = traceBot.Token
	req.TraceTelegramChatIDs = traceBot.ChatIDs

	runnerPrompt := appendImageContext(prompt, images)
	if err := b.app.Runner().Prompt(ctx, req, runnerPrompt, images); err != nil {
		b.send(chatID, "Failed to start pi: "+err.Error(), nil)
		return
	}
	clearTraceRetry(b, userID)
	b.send(chatID, "Sent to session.", taskKeyboard(ws.ID, b.pinned(userID) == ws.Path))
}
