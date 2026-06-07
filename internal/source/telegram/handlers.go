package telegram

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xiaot/pi-coordinator/internal/gitops"
	"github.com/xiaot/pi-coordinator/internal/runner"
	"github.com/xiaot/pi-coordinator/internal/store"
)

// registerHandlers sets up all routes for the Telegram bot.
func (b *Bot) registerHandlers() {
	// Commands
	b.router.Command("help", handleHelp)
	b.router.Command("sync", handleSync)
	b.router.Command("workspace", handleWorkspaceCmd)
	b.router.Command("add", handleAddCmd)
	b.router.Command("new", handleNewCmd)
	b.router.Command("status", handleStatusCmd)
	b.router.Command("open", handleOpenCmd)
	b.router.Command("rebase", handleRebaseCmd)
	b.router.Command("commit", handleCommitCmd)
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
	b.router.Callback("wsopen:", handleWorkspaceOpen)
	b.router.Callback("wsmodel:", handleWorkspaceModel)
	b.router.Callback("wspin:", handleWorkspacePin)

	b.router.Callback("wp:", handleWorkspacePage)
	b.router.Callback("newwsp:", handleNewWorkspacePage)
	b.router.Callback("pinwsp:", handlePinWorkspacePage)
	b.router.Callback("w:", handleChooseWorkspaceForSession)
	b.router.Callback("newws:", handleChooseWorkspaceForNewTask)
	b.router.Callback("pinws:", handleChooseWorkspaceForPin)
	b.router.Callback("add:confirm", handleAddConfirm)
	b.router.Callback("add:cancel", handleAddCancel)
	b.router.Callback("add:up", handleAddUp)
	b.router.Callback("add:toggle", handleAddToggle)
	b.router.Callback("add:page:", handleAddPage)
	b.router.Callback("add:open:", handleAddOpen)

	b.router.Callback("ns:", handleNewSessionCallback)
	b.router.Callback("runlocal:", handleRunLocal)
	b.router.Callback("runworktree:", handleRunWorktree)
	b.router.Callback("rundocker:", handleRunDocker)
	b.router.Callback("s:", handleSessionCallback)
	b.router.Callback("sp:", handleSessionPage)
	b.router.Callback("sessions:", handleSessionsList)
	b.router.Callback("status:", handleStatusCallback)
	b.router.Callback("new:", handleNewCallback)
	b.router.Callback("pin:", handlePinCallback)
	b.router.Callback("trace:retry", handleTraceRetry)
}

// -- Command Handlers --

func handleHelp(ctx context.Context, b *Bot, update Update) {
	b.send(update.Message.Chat.ID, "pico is ready. Use /sync to import history, /new to start a task, /status to inspect active sessions, and /rebase or /commit inside a session topic.", nil)
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

func handleAddCmd(ctx context.Context, b *Bot, update Update) {
	startAddWorkspaceBrowser(ctx, b, update.Message.Chat.ID, 0, update.Message.From.ID)
}

func handleNewCmd(ctx context.Context, b *Bot, update Update) {
	prompt := strings.TrimSpace(update.Message.CommandArguments())
	if path := b.pinned(update.Message.From.ID); path != "" {
		ws, err := b.app.GetSelectableWorkspaceByPath(ctx, path)
		if err != nil {
			b.send(update.Message.Chat.ID, "Pinned workspace is unavailable: "+err.Error(), nil)
			return
		}
		if prompt != "" {
			startNewTask(ctx, b, update.Message.Chat, update.Message.From.ID, ws.ID, prompt, nil)
			return
		}
		promptForPinnedNewTask(b, update.Message.Chat, ws)
		return
	}
	b.setPending(update.Message.From.ID, PendingState{Kind: "new_prompt", Prompt: prompt})
	sendWorkspaces(ctx, b, update.Message.Chat.ID, 0, "Choose a workspace:", "newws:", 0)
}

func handleOpenCmd(ctx context.Context, b *Bot, update Update) {
	msg := update.Message
	sess, ws, ok := sessionTopicCommandContext(ctx, b, msg, "/open")
	if !ok {
		return
	}
	tool := strings.TrimSpace(b.app.Config().OpenTool)
	if tool == "" {
		tool = "iterm2"
	}
	openPath, openLabel, err := sessionOpenPath(sess, ws)
	if err != nil {
		b.sendMessage(msg.Chat.ID, msg.MessageThreadID, "Failed to resolve workspace to open: "+err.Error(), nil)
		return
	}
	if err := openWorkspace(ctx, tool, openPath); err != nil {
		b.sendMessage(msg.Chat.ID, msg.MessageThreadID, "Failed to open workspace with "+tool+": "+err.Error(), nil)
		return
	}
	b.sendMessage(msg.Chat.ID, msg.MessageThreadID, "Opened "+openLabel+" with "+tool+": "+openPath, nil)
}

func handleRebaseCmd(ctx context.Context, b *Bot, update Update) {
	msg := update.Message
	sess, ws, ok := gitSessionCommandContext(ctx, b, msg, "/rebase")
	if !ok {
		return
	}
	result, err := gitops.Run(ctx, "rebase.sh", map[string]string{
		"PI_ORIGINAL_WORKSPACE": originalWorkspacePath(sess, ws),
		"PI_SESSION_ID":         sess.ID,
		"PI_WORKTREE_PATH":      sess.WorktreePath,
	})
	if err != nil {
		b.sendMessage(msg.Chat.ID, msg.MessageThreadID, "Rebase failed: "+err.Error()+"\nWorktree: "+sess.WorktreePath, nil)
		return
	}
	baseBranch, _ := gitops.RequireValue(result.Values, "BASE_BRANCH")
	headSHA, _ := gitops.RequireValue(result.Values, "HEAD_SHA")
	text := fmt.Sprintf("Rebased onto %s.\nWorktree: %s\nHEAD: %s", baseBranch, sess.WorktreePath, headSHA)
	if gitops.BoolValue(result.Values, "STASHED") {
		text += "\nStashed and restored local changes."
	}
	b.sendMessage(msg.Chat.ID, msg.MessageThreadID, text, nil)
}

func handleCommitCmd(ctx context.Context, b *Bot, update Update) {
	msg := update.Message
	rawMessage := strings.TrimSpace(msg.CommandArguments())
	if rawMessage == "" {
		b.sendMessage(msg.Chat.ID, msg.MessageThreadID, "Usage: /commit <msg>\n<detail can be empty or multi-line>", nil)
		return
	}
	sess, ws, ok := gitSessionCommandContext(ctx, b, msg, "/commit")
	if !ok {
		return
	}
	result, err := gitops.Run(ctx, "commit.sh", map[string]string{
		"PI_COMMIT_MESSAGE_RAW": rawMessage,
		"PI_ORIGINAL_WORKSPACE": originalWorkspacePath(sess, ws),
		"PI_SESSION_ID":         sess.ID,
		"PI_WORKTREE_BRANCH":    sess.WorktreeBranch,
		"PI_WORKTREE_PATH":      sess.WorktreePath,
	})
	if err != nil {
		b.sendMessage(msg.Chat.ID, msg.MessageThreadID, "Commit failed: "+err.Error()+"\nWorktree: "+sess.WorktreePath, nil)
		return
	}
	baseBranch, _ := gitops.RequireValue(result.Values, "BASE_BRANCH")
	headSHA, _ := gitops.RequireValue(result.Values, "HEAD_SHA")
	text := fmt.Sprintf("Committed to %s.\nWorkspace: %s\nWorktree: %s\nHEAD: %s", baseBranch, originalWorkspacePath(sess, ws), sess.WorktreePath, headSHA)
	if gitops.BoolValue(result.Values, "CREATED_COMMIT") {
		text += "\nCreated a new commit from current worktree changes."
	}
	b.sendMessage(msg.Chat.ID, msg.MessageThreadID, text, nil)
}

func sessionTopicCommandContext(ctx context.Context, b *Bot, msg *Message, command string) (store.Session, store.Workspace, bool) {
	if msg.Chat.ID != b.app.Config().Telegram.GroupChatID || msg.MessageThreadID == 0 || msg.MessageThreadID == generalTopicThreadID {
		b.send(msg.Chat.ID, command+" is only available inside a session topic.", nil)
		return store.Session{}, store.Workspace{}, false
	}
	sess, err := b.app.Store().GetSessionByTopic(ctx, msg.MessageThreadID)
	if err != nil {
		b.sendMessage(msg.Chat.ID, msg.MessageThreadID, "No session is linked to this topic.", nil)
		return store.Session{}, store.Workspace{}, false
	}
	ws, err := b.app.Store().GetWorkspace(ctx, sess.WorkspaceID)
	if err != nil {
		b.sendMessage(msg.Chat.ID, msg.MessageThreadID, "Failed to read workspace: "+err.Error(), nil)
		return store.Session{}, store.Workspace{}, false
	}
	return sess, ws, true
}

func gitSessionCommandContext(ctx context.Context, b *Bot, msg *Message, command string) (store.Session, store.Workspace, bool) {
	sess, ws, ok := sessionTopicCommandContext(ctx, b, msg, command)
	if !ok {
		return store.Session{}, store.Workspace{}, false
	}
	if sess.RunnerType != "worktree" && sess.RunnerType != "docker" {
		b.sendMessage(msg.Chat.ID, msg.MessageThreadID, command+" is only available for worktree and docker sessions.", nil)
		return store.Session{}, store.Workspace{}, false
	}
	if sess.WorktreePath == "" || sess.WorktreeBranch == "" {
		b.sendMessage(msg.Chat.ID, msg.MessageThreadID, command+" requires a session worktree, but this session has no worktree metadata.", nil)
		return store.Session{}, store.Workspace{}, false
	}
	return sess, ws, true
}

func originalWorkspacePath(sess store.Session, ws store.Workspace) string {
	if strings.TrimSpace(sess.OriginalWorkspacePath) != "" {
		return sess.OriginalWorkspacePath
	}
	return ws.Path
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

func modelCommandTopicID(msg *Message) int {
	if msg == nil || msg.Chat.IsPrivate() {
		return 0
	}
	return msg.MessageThreadID
}

func currentTopicSession(ctx context.Context, b *Bot, msg *Message) (store.Session, bool) {
	if msg == nil || msg.Chat.ID != b.app.Config().Telegram.GroupChatID || msg.MessageThreadID == 0 || msg.MessageThreadID == generalTopicThreadID {
		return store.Session{}, false
	}
	sess, err := b.app.Store().GetSessionByTopic(ctx, msg.MessageThreadID)
	if err != nil {
		return store.Session{}, false
	}
	return sess, true
}

func modelPendingForScope(ctx context.Context, b *Bot, userID int64, msg *Message, scope string) PendingState {
	p := PendingState{Kind: "model", ModelScope: scope}
	if sess, ok := currentTopicSession(ctx, b, msg); ok {
		switch scope {
		case "workspace":
			p.WorkspaceID = sess.WorkspaceID
		case "session":
			p.WorkspaceID = sess.WorkspaceID
			p.SessionID = sess.ID
		}
		return p
	}
	if scope == "workspace" {
		if path := b.pinned(userID); path != "" {
			if ws, err := b.app.GetSelectableWorkspaceByPath(ctx, path); err == nil {
				p.WorkspaceID = ws.ID
			}
		}
	}
	return p
}

func defaultModelPending(ctx context.Context, b *Bot, msg *Message) (PendingState, bool) {
	if msg == nil || msg.From == nil {
		return PendingState{}, false
	}
	if sess, ok := currentTopicSession(ctx, b, msg); ok {
		return PendingState{Kind: "model", ModelScope: "session", WorkspaceID: sess.WorkspaceID, SessionID: sess.ID}, true
	}
	p := modelPendingForScope(ctx, b, msg.From.ID, msg, "workspace")
	if p.WorkspaceID != 0 {
		return p, true
	}
	return PendingState{}, false
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
	if p, ok := defaultModelPending(ctx, b, update.Message); ok {
		b.setPending(update.Message.From.ID, p)
		messageID, err := b.sendMessage(update.Message.Chat.ID, modelCommandTopicID(update.Message), "Loading models...", nil)
		if err != nil {
			b.app.Logger().Warn("telegram send failed", "error", err)
			return
		}
		editModelProviders(ctx, b, update.Message.Chat.ID, messageID, p.ModelScope)
		return
	}
	if _, err := b.sendMessage(update.Message.Chat.ID, modelCommandTopicID(update.Message), modelScopeText(refresh), modelScopeKeyboard()); err != nil {
		b.app.Logger().Warn("telegram send failed", "error", err)
	}
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
		ws, err := b.app.GetSelectableWorkspaceByPath(ctx, path)
		if err != nil {
			b.send(chatID, "Pinned workspace is unavailable: "+err.Error(), nil)
			return
		}
		startNewTask(ctx, b, msg.Chat, userID, ws.ID, text, images)
		return
	}
	if !msg.Chat.IsPrivate() {
		// General-topic prompts should be sent as regular group messages.
		// Using message_thread_id for the General topic can trigger 400 Bad Request
		// in some Telegram forum setups.
		b.send(chatID, "Choose a workspace with /new or /workspace, or /pin one for quick sends.", nil)
		return
	}
	b.send(chatID, "Choose a workspace first with /new or /workspace.", nil)
}

func promptForPendingInput(b *Bot, chat Chat, userID int64, pending PendingState, text string) {
	if _, err := b.sendMessage(chat.ID, 0, text, nil); err != nil {
		b.app.Logger().Warn("telegram send failed", "error", err)
		return
	}
	b.setPending(userID, pending)
}

func promptForNewTaskInput(ctx context.Context, b *Bot, chat Chat, userID, workspaceID int64) {
	ws, err := b.app.GetSelectableWorkspace(ctx, workspaceID)
	if err != nil {
		b.send(chat.ID, "Failed to read workspace: "+err.Error(), nil)
		return
	}
	text := "Send the task description."
	if chat.IsPrivate() {
		text = "Send the task for " + workspaceLabel(ws) + "."
	} else {
		text = "Send in General Topic to create a session in " + workspaceLabel(ws) + "."
	}
	promptForPendingInput(b, chat, userID, PendingState{Kind: "await_new_prompt", WorkspaceID: workspaceID}, text)
}

func promptForResumeInput(ctx context.Context, b *Bot, chat Chat, userID int64, sessionID string) {
	sess, err := b.app.Store().GetSession(ctx, sessionID)
	if err != nil {
		b.send(chat.ID, "Failed to read session: "+err.Error(), nil)
		return
	}
	text := "Send the message to continue this session."
	if chat.IsPrivate() {
		text = "Send the next message for " + displaySession(sess) + "."
	} else {
		text = "Send in General Topic to continue " + displaySession(sess) + "."
	}
	promptForPendingInput(b, chat, userID, PendingState{Kind: "await_resume_prompt", SessionID: sessionID}, text)
}

func promptForPinnedNewTask(b *Bot, chat Chat, ws store.Workspace) {
	text := "Send the task description."
	if chat.IsPrivate() {
		text = "Send the task for " + workspaceLabel(ws) + "."
	} else {
		text = "Send in General Topic to create a session in " + workspaceLabel(ws) + "."
	}
	if _, err := b.sendMessage(chat.ID, 0, text, nil); err != nil {
		b.app.Logger().Warn("telegram send failed", "error", err)
	}
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
	if sess.RunnerType == "" && sess.FilePath == "" && sess.WorktreePath == "" {
		b.sendMessage(msg.Chat.ID, msg.MessageThreadID, "Choose Run Local, Run Worktree, or Run Docker before sending follow-ups.", nil)
		return
	}
	if err := b.app.SteerSession(ctx, sess, ws, req, runnerPrompt, images); err != nil {
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
	b.setPending(q.From.ID, modelPendingForScope(ctx, b, q.From.ID, q.Message, scope))
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
		if p.WorkspaceID != 0 {
			applyWorkspaceModel(ctx, b, q.From.ID, q.Message.Chat.ID, q.Message.MessageID, p.WorkspaceID, p.ModelID)
			return
		}
		sendWorkspaces(ctx, b, q.Message.Chat.ID, q.Message.MessageID, "Choose workspace for "+modelDisplay(model)+":", "mws:", 0)
	case "session":
		if p.SessionID != "" {
			applySessionModel(ctx, b, q.From.ID, q.Message.Chat.ID, q.Message.MessageID, p.SessionID, p.ModelID)
			return
		}
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
	applyWorkspaceModel(ctx, b, q.From.ID, q.Message.Chat.ID, q.Message.MessageID, id, p.ModelID)
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
	applySessionModel(ctx, b, q.From.ID, q.Message.Chat.ID, q.Message.MessageID, sessionID, p.ModelID)
}

// -- Callbacks (Workspace & Sessions) --

func applyWorkspaceModel(ctx context.Context, b *Bot, userID, chatID int64, messageID int, workspaceID int64, modelID string) {
	ws, err := b.app.GetSelectableWorkspace(ctx, workspaceID)
	if err != nil {
		b.editMessageText(chatID, messageID, "Failed to read workspace: "+err.Error(), nil)
		return
	}
	if err := b.app.Store().SetWorkspaceModel(ctx, workspaceID, modelID); err != nil {
		b.editMessageText(chatID, messageID, "Failed to update workspace model: "+err.Error(), nil)
		return
	}
	b.clearPending(userID)
	b.editMessageText(chatID, messageID, fmt.Sprintf("Workspace model set to %s for %s.", modelID, workspaceLabel(ws)), nil)
}

func applySessionModel(ctx context.Context, b *Bot, userID, chatID int64, messageID int, sessionID, modelID string) {
	sess, err := b.app.Store().GetSession(ctx, sessionID)
	if err != nil {
		b.editMessageText(chatID, messageID, "Failed to read session: "+err.Error(), nil)
		return
	}
	if err := b.app.Store().SetSessionModel(ctx, sessionID, modelID); err != nil {
		b.editMessageText(chatID, messageID, "Failed to update session model: "+err.Error(), nil)
		return
	}
	b.clearPending(userID)
	b.editMessageText(chatID, messageID, fmt.Sprintf("Session model set to %s for %s.", modelID, displaySession(sess)), nil)
}

func handleWorkspaceOpen(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	id, _ := strconv.ParseInt(strings.TrimPrefix(q.Data, "wsopen:"), 10, 64)
	ws, err := b.app.GetSelectableWorkspace(ctx, id)
	if err != nil {
		b.answerCallback(q.ID, "Open failed")
		b.send(q.Message.Chat.ID, "Failed to read workspace: "+err.Error(), nil)
		return
	}
	tool := strings.TrimSpace(b.app.Config().OpenTool)
	if tool == "" {
		tool = "iterm2"
	}
	if err := openWorkspace(ctx, tool, ws.Path); err != nil {
		b.answerCallback(q.ID, "Open failed")
		b.send(q.Message.Chat.ID, "Failed to open workspace with "+tool+": "+err.Error(), nil)
		return
	}
	b.answerCallback(q.ID, "Opened with "+tool)
}

func handleWorkspaceModel(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	id, _ := strconv.ParseInt(strings.TrimPrefix(q.Data, "wsmodel:"), 10, 64)
	if _, err := b.app.GetSelectableWorkspace(ctx, id); err != nil {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Failed to read workspace: "+err.Error(), nil)
		return
	}
	b.setPending(q.From.ID, PendingState{Kind: "model", ModelScope: "workspace", WorkspaceID: id})
	editModelProviders(ctx, b, q.Message.Chat.ID, q.Message.MessageID, "workspace")
}

func handleWorkspacePin(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	id, _ := strconv.ParseInt(strings.TrimPrefix(q.Data, "wspin:"), 10, 64)
	ws, err := b.app.GetSelectableWorkspace(ctx, id)
	if err != nil {
		b.answerCallback(q.ID, "Pin failed")
		b.send(q.Message.Chat.ID, "Pin failed: "+err.Error(), nil)
		return
	}
	pinWorkspace(ctx, b, q.From.ID, q.Message.Chat.ID, q.Message.MessageThreadID, ws)
	b.answerCallback(q.ID, "Pinned "+workspaceLabel(ws))
}

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
	promptForNewTaskInput(ctx, b, q.Message.Chat, q.From.ID, id)
}

func handleChooseWorkspaceForPin(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	id, _ := strconv.ParseInt(strings.TrimPrefix(q.Data, "pinws:"), 10, 64)
	ws, err := b.app.GetSelectableWorkspace(ctx, id)
	if err != nil {
		b.send(q.Message.Chat.ID, "Pin failed: "+err.Error(), nil)
		return
	}
	pinWorkspace(ctx, b, q.From.ID, q.Message.Chat.ID, q.Message.MessageThreadID, ws)
}

func handleAddConfirm(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	p, ok := b.getPending(q.From.ID)
	if !ok || p.Kind != "add_workspace" || p.BrowsePath == "" {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Workspace add expired. Send /add again.", nil)
		return
	}
	if b.app.IsManagedWorktreePath(p.BrowsePath) {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Cannot add a pi-managed worktree as a workspace.", nil)
		return
	}
	ws, created, err := b.app.Store().UpsertWorkspace(ctx, p.BrowsePath, filepath.Base(p.BrowsePath))
	if err != nil {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Failed to add workspace: "+err.Error(), nil)
		return
	}
	b.clearPending(q.From.ID)
	if created {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Workspace added: "+ws.Path, taskKeyboard(ws.ID, b.pinned(q.From.ID) == ws.Path))
		return
	}
	b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Workspace already exists: "+ws.Path, taskKeyboard(ws.ID, b.pinned(q.From.ID) == ws.Path))
}

func handleAddCancel(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	b.clearPending(q.From.ID)
	b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Workspace add cancelled.", nil)
}

func handleAddUp(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	p, ok := b.getPending(q.From.ID)
	if !ok || p.Kind != "add_workspace" || p.BrowsePath == "" {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Workspace add expired. Send /add again.", nil)
		return
	}
	parent := filepath.Dir(p.BrowsePath)
	sendAddWorkspaceBrowser(ctx, b, q.Message.Chat.ID, q.Message.MessageID, q.From.ID, parent, 0, p.BrowseShowHidden)
}

func handleAddToggle(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	p, ok := b.getPending(q.From.ID)
	if !ok || p.Kind != "add_workspace" || p.BrowsePath == "" {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Workspace add expired. Send /add again.", nil)
		return
	}
	sendAddWorkspaceBrowser(ctx, b, q.Message.Chat.ID, q.Message.MessageID, q.From.ID, p.BrowsePath, 0, !p.BrowseShowHidden)
}

func handleAddPage(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	p, ok := b.getPending(q.From.ID)
	if !ok || p.Kind != "add_workspace" || p.BrowsePath == "" {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Workspace add expired. Send /add again.", nil)
		return
	}
	page, _ := strconv.Atoi(strings.TrimPrefix(q.Data, "add:page:"))
	sendAddWorkspaceBrowser(ctx, b, q.Message.Chat.ID, q.Message.MessageID, q.From.ID, p.BrowsePath, page, p.BrowseShowHidden)
}

func handleAddOpen(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	p, ok := b.getPending(q.From.ID)
	if !ok || p.Kind != "add_workspace" || p.BrowsePath == "" {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Workspace add expired. Send /add again.", nil)
		return
	}
	index, _ := strconv.Atoi(strings.TrimPrefix(q.Data, "add:open:"))
	dirs, _, err := childDirectories(p.BrowsePath, p.BrowseShowHidden)
	if err != nil {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Failed to read directory: "+err.Error(), nil)
		return
	}
	absoluteIndex := p.BrowsePage*addWorkspacePageSize + index
	if absoluteIndex < 0 || absoluteIndex >= len(dirs) {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Directory selection expired. Send /add again.", nil)
		return
	}
	sendAddWorkspaceBrowser(ctx, b, q.Message.Chat.ID, q.Message.MessageID, q.From.ID, dirs[absoluteIndex].Path, 0, p.BrowseShowHidden)
}

func handleNewSessionCallback(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	id, _ := strconv.ParseInt(strings.TrimPrefix(q.Data, "ns:"), 10, 64)
	promptForNewTaskInput(ctx, b, q.Message.Chat, q.From.ID, id)
}

func handleRunLocal(ctx context.Context, b *Bot, update Update) {
	handleRunMode(ctx, b, update, "local")
}

func handleRunWorktree(ctx context.Context, b *Bot, update Update) {
	handleRunMode(ctx, b, update, "worktree")
}

func handleRunDocker(ctx context.Context, b *Bot, update Update) {
	handleRunMode(ctx, b, update, "docker")
}

func handleSessionCallback(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	sid := strings.TrimPrefix(q.Data, "s:")
	promptForResumeInput(ctx, b, q.Message.Chat, q.From.ID, sid)
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
	promptForNewTaskInput(ctx, b, q.Message.Chat, q.From.ID, id)
}

func handlePinCallback(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	id, _ := strconv.ParseInt(strings.TrimPrefix(q.Data, "pin:"), 10, 64)
	ws, err := b.app.GetSelectableWorkspace(ctx, id)
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
		if ws, err := b.app.GetSelectableWorkspace(ctx, id); err == nil {
			return ws, nil
		}
	}
	if ws, err := b.app.GetSelectableWorkspaceByPath(ctx, query); err == nil {
		return ws, nil
	}
	total, err := b.app.CountSelectableWorkspaces(ctx)
	if err != nil {
		return store.Workspace{}, err
	}
	workspaces, err := b.app.ListSelectableWorkspaces(ctx, total, 0)
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
	ws, err := b.app.GetSelectableWorkspace(ctx, workspaceID)
	if err != nil {
		b.send(chatID, "Failed to read workspace: "+err.Error(), nil)
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

	b.setPending(userID, PendingState{
		Kind:         "await_run_mode",
		WorkspaceID:  workspaceID,
		SessionID:    sess.ID,
		Prompt:       prompt,
		ImagesJSON:   encodeImagesJSON(images),
		TaskChatID:   chat.ID,
		TaskChatType: chat.Type,
	})
	gitWorkspace := b.app.IsGitWorkspace(ctx, ws)
	text := fmt.Sprintf("Created topic: %s\nChoose how to run pi.", title)
	if gitWorkspace && b.app.HasDirtyChanges(ctx, ws) {
		text += "\n\nWorktree and Docker will start from the current HEAD and will not include uncommitted changes in the original workspace."
	}
	b.send(chatID, text, createdTopicKeyboard(sess.ID, gitWorkspace))
}

func handleRunMode(ctx context.Context, b *Bot, update Update, runnerType string) {
	q := update.CallbackQuery
	sessionID := strings.TrimPrefix(q.Data, "run"+runnerType+":")
	if runnerType == "local" {
		sessionID = strings.TrimPrefix(q.Data, "runlocal:")
	}
	if runnerType == "worktree" {
		sessionID = strings.TrimPrefix(q.Data, "runworktree:")
	}
	if runnerType == "docker" {
		sessionID = strings.TrimPrefix(q.Data, "rundocker:")
	}
	p, ok := b.getPending(q.From.ID)
	if !ok || p.Kind != "await_run_mode" || p.SessionID != sessionID {
		b.send(q.Message.Chat.ID, "Run request expired. Send the task again with /new.", nil)
		return
	}
	sess, err := b.app.Store().GetSession(ctx, sessionID)
	if err != nil {
		b.send(q.Message.Chat.ID, "Failed to read session: "+err.Error(), nil)
		return
	}
	ws, err := b.app.Store().GetWorkspace(ctx, sess.WorkspaceID)
	if err != nil {
		b.send(q.Message.Chat.ID, "Failed to read workspace: "+err.Error(), nil)
		return
	}
	if runnerType != "local" && !b.app.IsGitWorkspace(ctx, ws) {
		b.send(q.Message.Chat.ID, "Worktree and Docker modes require a Git workspace with at least one commit.", nil)
		return
	}
	chat := Chat{ID: p.TaskChatID, Type: p.TaskChatType}
	if chat.ID == 0 {
		chat = q.Message.Chat
	}
	rememberTraceRetry(b, q.From.ID, "retry_new_task", chat, ws.ID, sess.ID, p.Prompt, decodeImagesJSON(p.ImagesJSON))
	traceBot, err := b.ensureTraceBot(ctx, genericRole, q.Message.Chat.ID)
	if err != nil {
		return
	}
	req := runner.StartRequest{
		SessionID: sess.ID,
		Title:     displaySession(sess),
		TopicID:   sess.TopicID,
		Model:     b.app.ResolveModel(sess, ws),
		Role:      genericRole,
	}
	req.TraceTelegramToken = traceBot.Token
	req.TraceTelegramChatIDs = traceBot.ChatIDs

	images := decodeImagesJSON(p.ImagesJSON)
	runnerPrompt := appendImageContext(p.Prompt, images)
	prepared, err := b.app.PromptSession(ctx, sess, ws, runnerType, req, runnerPrompt, images)
	if err != nil {
		b.send(q.Message.Chat.ID, "Failed to start pi: "+err.Error(), nil)
		return
	}
	clearTraceRetry(b, q.From.ID)
	text := "Started pi in " + runnerType + " mode."
	if prepared.WorktreePath != "" {
		text += "\nWorktree: " + prepared.WorktreePath
	}
	b.send(q.Message.Chat.ID, text, startedTaskKeyboard(ws.ID, b.pinned(q.From.ID) == ws.Path, b.app.Config().Telegram.GroupChatID, sess.TopicID))
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
		TopicID:   sess.TopicID,
		Model:     b.app.ResolveModel(sess, ws),
		Existing:  true,
		Role:      genericRole,
	}
	req.TraceTelegramToken = traceBot.Token
	req.TraceTelegramChatIDs = traceBot.ChatIDs

	runnerPrompt := appendImageContext(prompt, images)
	if _, err := b.app.PromptSession(ctx, sess, ws, sess.RunnerType, req, runnerPrompt, images); err != nil {
		b.send(chatID, "Failed to start pi: "+err.Error(), nil)
		return
	}
	clearTraceRetry(b, userID)
	b.send(chatID, "Sent to session.", taskKeyboard(ws.ID, b.pinned(userID) == ws.Path))
}
