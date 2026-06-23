package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xiaot/pi-coordinator/internal/crons"
	"github.com/xiaot/pi-coordinator/internal/runner"
	"github.com/xiaot/pi-coordinator/internal/store"
)

func continueCronCreateWithSchedule(ctx context.Context, b *Bot, chatID int64, chat Chat, userID int64, p PendingState, schedule string) {
	schedule = strings.TrimSpace(schedule)
	if _, err := crons.Parse(schedule); err != nil {
		b.send(chatID, "Invalid cron schedule: "+err.Error()+"\nSend a 5-field schedule, for example: 0 9 * * *", nil)
		b.setPending(userID, p)
		return
	}
	p.Kind = "await_cron_prompt"
	p.CronSchedule = schedule
	prompt := "Send the cron task content. The first line becomes the title."
	if !chat.IsPrivate() && chat.ID == b.app.Config().Telegram.GroupChatID && chat.Type != "private" {
		prompt = "Send the cron task content in General Topic or private chat. The first line becomes the title."
	}
	promptForPendingInput(b, chat, userID, p, prompt)
}

func createCronFromPending(ctx context.Context, b *Bot, chatID int64, p PendingState, text string) {
	workspaceKind := ""
	workspaceID := p.WorkspaceID
	if p.Temporary {
		workspaceKind = crons.WorkspaceKindTemporary
		workspaceID = 0
	}
	item, err := b.app.CronStore().Create(crons.CreateInput{
		WorkspaceID:   workspaceID,
		WorkspaceKind: workspaceKind,
		Prompt:        text,
		Schedule:      p.CronSchedule,
		Mode:          p.CronMode,
		Runner:        p.CronRunner,
	})
	if err != nil {
		b.send(chatID, "Failed to create cron: "+err.Error(), nil)
		return
	}
	sendCronItemDetail(ctx, b, p.TaskChatID, p.MessageID, item, p.Page)
}

func updateCronFromPending(ctx context.Context, b *Bot, chatID int64, p PendingState, text string) {
	item, err := b.app.CronStore().UpdatePrompt(p.CronID, text)
	if err != nil {
		b.send(chatID, "Failed to update cron: "+err.Error(), nil)
		return
	}
	sendCronItemDetail(ctx, b, p.TaskChatID, p.MessageID, item, p.Page)
}

func handleCronWorkspacePage(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	page, _ := strconv.Atoi(strings.TrimPrefix(q.Data, "cron:wp:"))
	sendCronWorkspaces(ctx, b, q.Message.Chat.ID, q.Message.MessageID, page)
}

func handleCronWorkspace(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	workspaceID, temporary, ok := parseCronWorkspaceKey(strings.TrimPrefix(q.Data, "cron:ws:"))
	if !ok {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Cron workspace is unavailable.", nil)
		return
	}
	sendCronList(ctx, b, q.Message.Chat.ID, q.Message.MessageID, workspaceID, temporary, 0)
}

func handleCronListPage(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	payload := strings.TrimPrefix(q.Data, "cron:list:")
	parts := strings.Split(payload, ":")
	if len(parts) != 2 {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Cron list expired. Send /cron again.", nil)
		return
	}
	workspaceID, temporary, ok := parseCronWorkspaceKey(parts[0])
	if !ok {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Cron workspace is unavailable.", nil)
		return
	}
	page, _ := strconv.Atoi(parts[1])
	sendCronList(ctx, b, q.Message.Chat.ID, q.Message.MessageID, workspaceID, temporary, page)
}

func handleCronAdd(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	workspaceID, temporary, ok := parseCronWorkspaceKey(strings.TrimPrefix(q.Data, "cron:add:"))
	if !ok {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Cron workspace is unavailable.", nil)
		return
	}
	sendCronModePicker(b, q.Message.Chat.ID, q.Message.MessageID, workspaceID, temporary)
}

func handleCronMode(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	payload := strings.TrimPrefix(q.Data, "cron:mode:")
	parts := strings.Split(payload, ":")
	if len(parts) != 2 {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Cron mode expired. Send /cron again.", nil)
		return
	}
	workspaceID, temporary, ok := parseCronWorkspaceKey(parts[0])
	if !ok || (parts[1] != crons.ModeAuto && parts[1] != crons.ModeManual) {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Cron mode is unavailable.", nil)
		return
	}
	sendCronRunnerPicker(ctx, b, q.Message.Chat.ID, q.Message.MessageID, workspaceID, temporary, parts[1])
}

func handleCronRunner(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	payload := strings.TrimPrefix(q.Data, "cron:runner:")
	parts := strings.Split(payload, ":")
	if len(parts) != 3 {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Cron runner expired. Send /cron again.", nil)
		return
	}
	workspaceID, temporary, ok := parseCronWorkspaceKey(parts[0])
	if !ok {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Cron workspace is unavailable.", nil)
		return
	}
	mode := parts[1]
	runnerType := parts[2]
	if mode != crons.ModeAuto && mode != crons.ModeManual {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Cron mode is unavailable.", nil)
		return
	}
	if err := validateCronRunner(ctx, b, workspaceID, temporary, runnerType); err != nil {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, err.Error(), nil)
		return
	}
	prompt := "Send a 5-field cron schedule. Example: 0 9 * * *"
	if !q.Message.Chat.IsPrivate() && q.Message.MessageThreadID != 0 && q.Message.MessageThreadID != generalTopicThreadID {
		prompt = "Send a 5-field cron schedule in General Topic or private chat. Example: 0 9 * * *"
	}
	pending := PendingState{
		Kind:        "await_cron_schedule",
		WorkspaceID: workspaceID,
		Temporary:   temporary,
		CronMode:    mode,
		CronRunner:  runnerType,
		TaskChatID:  q.Message.Chat.ID,
		MessageID:   q.Message.MessageID,
		Page:        0,
	}
	promptForPendingInput(b, q.Message.Chat, q.From.ID, pending, prompt)
}

func handleCronItem(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	payload := strings.TrimPrefix(q.Data, "cron:item:")
	parts := strings.Split(payload, ":")
	if len(parts) != 2 {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Cron item expired. Send /cron again.", nil)
		return
	}
	page, _ := strconv.Atoi(parts[1])
	item, err := b.app.CronStore().Get(parts[0])
	if err != nil {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Failed to read cron: "+err.Error(), nil)
		return
	}
	sendCronItemDetail(ctx, b, q.Message.Chat.ID, q.Message.MessageID, item, page)
}

func handleCronRun(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	payload := strings.TrimPrefix(q.Data, "cron:run:")
	parts := strings.Split(payload, ":")
	if len(parts) != 2 {
		b.send(q.Message.Chat.ID, "Cron run request expired. Send /cron again.", nil)
		return
	}
	item, err := b.app.CronStore().Get(parts[0])
	if err != nil {
		b.send(q.Message.Chat.ID, "Failed to read cron: "+err.Error(), nil)
		return
	}
	runnerType := parts[1]
	if runnerType == "" {
		runnerType = item.Runner
	}
	if err := startCronSession(ctx, b, item, runnerType, q.From.ID); err != nil {
		b.send(q.Message.Chat.ID, "Failed to run cron: "+err.Error(), nil)
		return
	}
	_ = b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Starting cron: "+cronTitleLabel(item.Title)+"\nRunner: "+runnerType, nil)
}

func handleCronToggle(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	payload := strings.TrimPrefix(q.Data, "cron:toggle:")
	parts := strings.Split(payload, ":")
	if len(parts) != 2 {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Cron item expired. Send /cron again.", nil)
		return
	}
	page, _ := strconv.Atoi(parts[1])
	item, err := b.app.CronStore().Get(parts[0])
	if err != nil {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Failed to read cron: "+err.Error(), nil)
		return
	}
	item, err = b.app.CronStore().SetEnabled(item.ID, !item.Enabled)
	if err != nil {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Failed to update cron: "+err.Error(), nil)
		return
	}
	sendCronItemDetail(ctx, b, q.Message.Chat.ID, q.Message.MessageID, item, page)
}

func handleCronEdit(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	payload := strings.TrimPrefix(q.Data, "cron:edit:")
	parts := strings.Split(payload, ":")
	if len(parts) != 2 {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Cron item expired. Send /cron again.", nil)
		return
	}
	page, _ := strconv.Atoi(parts[1])
	item, err := b.app.CronStore().Get(parts[0])
	if err != nil {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Failed to read cron: "+err.Error(), nil)
		return
	}
	prompt := "Send the updated cron task content. The first line becomes the title.\n\nCurrent content:\n" + item.Prompt
	if !q.Message.Chat.IsPrivate() && q.Message.MessageThreadID != 0 && q.Message.MessageThreadID != generalTopicThreadID {
		prompt = "Send the updated cron task content in General Topic or private chat. The first line becomes the title.\n\nCurrent content:\n" + item.Prompt
	}
	pending := PendingState{
		Kind:       "await_cron_edit",
		CronID:     item.ID,
		TaskChatID: q.Message.Chat.ID,
		MessageID:  q.Message.MessageID,
		Page:       page,
	}
	promptForPendingInput(b, q.Message.Chat, q.From.ID, pending, prompt)
}

func handleCronDelete(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	payload := strings.TrimPrefix(q.Data, "cron:del:")
	parts := strings.Split(payload, ":")
	if len(parts) != 2 {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Cron item expired. Send /cron again.", nil)
		return
	}
	page, _ := strconv.Atoi(parts[1])
	item, err := b.app.CronStore().Delete(parts[0])
	if err != nil {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Failed to delete cron: "+err.Error(), nil)
		return
	}
	sendCronList(ctx, b, q.Message.Chat.ID, q.Message.MessageID, item.WorkspaceID, item.IsTemporary(), page)
}

func handleCronSkip(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	id := strings.TrimPrefix(q.Data, "cron:skip:")
	item, err := b.app.CronStore().Get(id)
	if err != nil {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Failed to read cron: "+err.Error(), nil)
		return
	}
	b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Skipped cron: "+cronTitleLabel(item.Title), nil)
}

func handleCronBack(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	payload := strings.TrimPrefix(q.Data, "cron:back:")
	parts := strings.Split(payload, ":")
	if len(parts) != 2 {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Cron list expired. Send /cron again.", nil)
		return
	}
	workspaceID, temporary, ok := parseCronWorkspaceKey(parts[0])
	if !ok {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Cron workspace is unavailable.", nil)
		return
	}
	page, _ := strconv.Atoi(parts[1])
	sendCronList(ctx, b, q.Message.Chat.ID, q.Message.MessageID, workspaceID, temporary, page)
}

func validateCronRunner(ctx context.Context, b *Bot, workspaceID int64, temporary bool, runnerType string) error {
	if err := crons.ValidateRunner(runnerType); err != nil {
		return err
	}
	if temporary {
		if runnerType != crons.RunnerDocker {
			return fmt.Errorf("temporary cron tasks only support Docker")
		}
		return nil
	}
	if runnerType == crons.RunnerLocal {
		return nil
	}
	ws, err := b.app.GetSelectableWorkspace(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("cron workspace is unavailable: %w", err)
	}
	if !b.app.IsGitWorkspace(ctx, ws) {
		return fmt.Errorf("Worktree and Docker modes require a Git workspace with at least one commit")
	}
	return nil
}

func resolveCronWorkspace(ctx context.Context, b *Bot, item crons.Item) (store.Workspace, error) {
	if item.IsTemporary() {
		return b.app.EnsureTemporaryWorkspace(ctx)
	}
	return b.app.GetSelectableWorkspace(ctx, item.WorkspaceID)
}

func startCronSession(ctx context.Context, b *Bot, item crons.Item, runnerType string, userID int64) error {
	if err := validateCronRunner(ctx, b, item.WorkspaceID, item.IsTemporary(), runnerType); err != nil {
		return err
	}
	ws, err := resolveCronWorkspace(ctx, b, item)
	if err != nil {
		return err
	}
	sess, err := b.app.Store().CreatePlaceholderSession(ctx, ws.ID, topicTitle(item.Prompt))
	if err != nil {
		return err
	}
	traceBot, err := b.ensureTraceBot(ctx, genericRole, b.app.Config().Telegram.GroupChatID)
	if err != nil {
		return err
	}
	sess, err = ensureSessionTopic(ctx, b, sess, item.Prompt)
	if err != nil {
		return err
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
	applyAgentTelegramEnv(ctx, b, &req, sess, traceBot.Token)
	prepared, err := b.app.PromptSession(ctx, sess, ws, runnerType, req, item.Prompt, nil)
	if err != nil {
		return err
	}
	text := "Started cron " + cronTitleLabel(item.Title) + " in " + runnerType + " mode."
	if b.app.IsTemporaryWorkspace(ws) {
		text = "Started cron " + cronTitleLabel(item.Title) + " in temporary docker mode.\nWorkspace mount: disabled"
	} else if prepared.WorktreePath != "" {
		text += "\nWorktree: " + prepared.WorktreePath
	}
	b.sendMessage(b.app.Config().Telegram.GroupChatID, 0, text, sessionReplyKeyboard(b, userID, ws, prepared.TopicID))
	return nil
}

func cronDueText(ctx context.Context, b *Bot, item crons.Item, auto bool) string {
	workspaceName, err := todoWorkspaceName(ctx, b, item.WorkspaceID, item.IsTemporary())
	if err != nil {
		workspaceName = "workspace unavailable"
	}
	prefix := "⏰ Cron due"
	if auto {
		prefix = "🤖 Cron due · auto starting"
	}
	return prefix +
		"\nTask: " + cronTitleLabel(item.Title) +
		"\nWorkspace: " + workspaceName +
		"\nSchedule: " + item.Schedule +
		"\nMode: " + item.Mode +
		"\nRunner: " + item.Runner
}

func handleCronDue(ctx context.Context, b *Bot, item crons.Item) {
	if item.Mode == crons.ModeManual {
		b.sendMessage(b.app.Config().Telegram.GroupChatID, 0, cronDueText(ctx, b, item, false), cronDueKeyboard(ctx, b, item))
		return
	}
	b.sendMessage(b.app.Config().Telegram.GroupChatID, 0, cronDueText(ctx, b, item, true), nil)
	if err := startCronSession(ctx, b, item, item.Runner, 0); err != nil {
		b.sendMessage(b.app.Config().Telegram.GroupChatID, 0, "Failed to start cron "+cronTitleLabel(item.Title)+": "+err.Error(), nil)
	}
}

func runCronScheduler(ctx context.Context, b *Bot) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	check := func() {
		now := time.Now()
		items, err := b.app.CronStore().ListEnabled()
		if err != nil {
			b.app.Logger().Warn("read cron tasks failed", "error", err)
			return
		}
		for _, item := range items {
			matched, err := crons.Matches(item.Schedule, now)
			if err != nil {
				b.app.Logger().Warn("parse cron schedule failed", "cron_id", item.ID, "schedule", item.Schedule, "error", err)
				continue
			}
			if !matched || sameLocalMinute(item.LastTriggeredAt, now) {
				continue
			}
			triggered, err := b.app.CronStore().MarkTriggered(item.ID, now)
			if err != nil {
				b.app.Logger().Warn("mark cron triggered failed", "cron_id", item.ID, "error", err)
				continue
			}
			go handleCronDue(ctx, b, triggered)
		}
	}
	check()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}

func sameLocalMinute(a time.Time, b time.Time) bool {
	if a.IsZero() {
		return false
	}
	la := a.Local().Truncate(time.Minute)
	lb := b.Local().Truncate(time.Minute)
	return la.Equal(lb)
}
