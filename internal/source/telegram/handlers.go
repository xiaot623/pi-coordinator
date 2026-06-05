package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/xiaot/pi-coordinator/internal/runner"
)

// registerHandlers sets up all routes for the Telegram bot.
func (b *Bot) registerHandlers() {
	// Commands
	b.router.Command("start", handleStart)
	b.router.Command("sync", handleSync)
	b.router.Command("workspace", handleWorkspaceCmd)
	b.router.Command("new", handleNewCmd)
	b.router.Command("unpin", handleUnpin)
	b.router.Command("model", handleModelCmd)

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
	b.router.Callback("w:", handleChooseWorkspaceForSession)
	b.router.Callback("newws:", handleChooseWorkspaceForNewTask)
	
	b.router.Callback("ns:", handleNewSessionCallback)
	b.router.Callback("s:", handleSessionCallback)
	b.router.Callback("sp:", handleSessionPage)
	b.router.Callback("sessions:", handleSessionsList)
	b.router.Callback("new:", handleNewCallback)
	b.router.Callback("pin:", handlePinCallback)
}

// -- Command Handlers --

func handleStart(ctx context.Context, b *Bot, update Update) {
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
	b.setPending(update.Message.From.ID, PendingState{Kind: "new_prompt", Prompt: prompt})
	sendWorkspaces(ctx, b, update.Message.Chat.ID, 0, "Choose a workspace:", "newws:", 0)
}

func handleUnpin(ctx context.Context, b *Bot, update Update) {
	b.clearPin(update.Message.From.ID)
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

// -- Message Handlers --

func handlePrivateMessage(ctx context.Context, b *Bot, update Update) {
	msg := update.Message
	text := strings.TrimSpace(msg.Text)
	userID := msg.From.ID
	chatID := msg.Chat.ID

	if p, ok := b.getPending(userID); ok {
		switch p.Kind {
		case "await_new_prompt":
			b.clearPending(userID)
			startNewTask(ctx, b, chatID, userID, p.WorkspaceID, text)
			return
		case "await_resume_prompt":
			b.clearPending(userID)
			resumeSession(ctx, b, chatID, userID, p.SessionID, text)
			return
		}
	}
	
	if path := b.pinned(userID); path != "" {
		ws, err := b.app.Store().GetWorkspaceByPath(ctx, path)
		if err != nil {
			b.send(chatID, "Pinned workspace is unavailable: "+err.Error(), nil)
			return
		}
		startNewTask(ctx, b, chatID, userID, ws.ID, text)
		return
	}
	b.send(chatID, "Choose a workspace first with /new or /workspace.", nil)
}

func handleTopicMessage(ctx context.Context, b *Bot, update Update) {
	msg := update.Message
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
	}
	
	if err := b.app.Runner().Steer(ctx, req, msg.Text); err != nil {
		b.sendMessage(msg.Chat.ID, msg.MessageThreadID, "Failed to send to pi: "+err.Error(), nil)
	}
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
		startNewTask(ctx, b, q.Message.Chat.ID, q.From.ID, id, p.Prompt)
		return
	}
	b.setPending(q.From.ID, PendingState{Kind: "await_new_prompt", WorkspaceID: id})
	b.send(q.Message.Chat.ID, "Send the task description.", nil)
}

func handleNewSessionCallback(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	id, _ := strconv.ParseInt(strings.TrimPrefix(q.Data, "ns:"), 10, 64)
	b.setPending(q.From.ID, PendingState{Kind: "await_new_prompt", WorkspaceID: id})
	b.send(q.Message.Chat.ID, "Send the task description.", nil)
}

func handleSessionCallback(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	sid := strings.TrimPrefix(q.Data, "s:")
	b.setPending(q.From.ID, PendingState{Kind: "await_resume_prompt", SessionID: sid})
	b.send(q.Message.Chat.ID, "Send the message to continue this session.", nil)
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
	b.setPending(q.From.ID, PendingState{Kind: "await_new_prompt", WorkspaceID: id})
	b.send(q.Message.Chat.ID, "Send the task description.", nil)
}

func handlePinCallback(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	id, _ := strconv.ParseInt(strings.TrimPrefix(q.Data, "pin:"), 10, 64)
	ws, err := b.app.Store().GetWorkspace(ctx, id)
	if err != nil {
		b.send(q.Message.Chat.ID, "Pin failed: "+err.Error(), nil)
		return
	}
	b.setPin(q.From.ID, ws.Path)
	b.send(q.Message.Chat.ID, fmt.Sprintf("📌 Workspace pinned: %s\nNew private messages will create tasks in this workspace.\nUse /unpin or /workspace to change it.", ws.Path), nil)
}

// -- Action Implementations --

func startNewTask(ctx context.Context, b *Bot, chatID, userID int64, workspaceID int64, prompt string) {
	ws, err := b.app.Store().GetWorkspace(ctx, workspaceID)
	if err != nil {
		b.send(chatID, "Failed to read workspace: "+err.Error(), nil)
		return
	}
	
	title := topicTitle(prompt)
	topicID, err := b.createForumTopic(title)
	if err != nil {
		b.send(chatID, "Failed to create topic: "+err.Error(), nil)
		return
	}
	
	goalID, err := b.sendTopicMessage(topicID, "🎯 "+prompt, nil)
	if err == nil {
		_ = b.pinChatMessage(goalID)
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
	}
	
	if err := b.app.Runner().Prompt(ctx, req, prompt); err != nil {
		b.send(chatID, "Failed to start pi: "+err.Error(), nil)
		return
	}
	
	b.send(chatID, fmt.Sprintf("Created topic: %s", title), taskKeyboard(workspaceID, b.pinned(userID) == ws.Path))
}

func resumeSession(ctx context.Context, b *Bot, chatID, userID int64, sessionID, prompt string) {
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
	
	if sess.TopicID == 0 {
		topicID, err := b.createForumTopic(topicTitle(prompt))
		if err != nil {
			b.send(chatID, "Failed to create topic: "+err.Error(), nil)
			return
		}
		goalID, _ := b.sendTopicMessage(topicID, "🎯 "+prompt, nil)
		_ = b.pinChatMessage(goalID)
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
	}
	
	if err := b.app.Runner().Prompt(ctx, req, prompt); err != nil {
		b.send(chatID, "Failed to start pi: "+err.Error(), nil)
		return
	}
	b.send(chatID, "Sent to session.", taskKeyboard(ws.ID, b.pinned(userID) == ws.Path))
}
