package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/xiaot/pi-coordinator/internal/gitops"
	"github.com/xiaot/pi-coordinator/internal/store"
)

const (
	detailTabOverview = "overview"
	detailTabGit      = "git"
	detailFileLimit   = 8
	detailPathLimit   = 72
)

type detailScope struct {
	Context      string
	Workspace    store.Workspace
	HasWorkspace bool
	Session      store.Session
	HasSession   bool
	Path         string
	RunMode      string
	State        string
	Model        string
	ModelSource  string
}

type detailGitReport struct {
	Available   bool
	RepoRoot    string
	Branch      string
	Head        string
	WorkingTree string
	DiffStat    string
	Files       []detailGitFile
}

type detailGitFile struct {
	Path      string
	Additions int
	Deletions int
	Status    string
}

func handleDetailCmd(ctx context.Context, b *Bot, update Update) {
	if args := strings.TrimSpace(update.Message.CommandArguments()); args != "" {
		detailRespond(b, update.Message.Chat.ID, detailResponseTopicID(update.Message), 0, "Usage: /detail", nil)
		return
	}
	sendDetailCard(ctx, b, update.Message.Chat.ID, detailResponseTopicID(update.Message), 0, update.Message.From.ID, update.Message, detailTabOverview)
}

func handleDetailCallback(ctx context.Context, b *Bot, update Update) {
	q := update.CallbackQuery
	if q == nil || q.Message == nil || q.From == nil {
		return
	}
	tab, ok := parseDetailTab(q.Data)
	if !ok {
		b.editMessageText(q.Message.Chat.ID, q.Message.MessageID, "Unknown detail action.", nil)
		return
	}
	sendDetailCard(ctx, b, q.Message.Chat.ID, 0, q.Message.MessageID, q.From.ID, q.Message, tab)
}

func sendDetailCard(ctx context.Context, b *Bot, chatID int64, topicID int, messageID int, userID int64, msg *Message, tab string) {
	scope, err := resolveDetailScope(ctx, b, msg, userID)
	if err != nil {
		detailRespond(b, chatID, topicID, messageID, "Failed to load detail: "+err.Error(), nil)
		return
	}
	text, err := renderDetailCard(ctx, scope, tab)
	if err != nil {
		detailRespond(b, chatID, topicID, messageID, "Failed to load detail: "+err.Error(), detailKeyboard(tab))
		return
	}
	detailRespond(b, chatID, topicID, messageID, text, detailKeyboard(tab))
}

func detailRespond(b *Bot, chatID int64, topicID int, messageID int, text string, replyMarkup any) {
	var err error
	if messageID == 0 {
		_, err = b.sendMessage(chatID, topicID, text, replyMarkup)
	} else {
		err = b.editMessageText(chatID, messageID, text, replyMarkup)
	}
	if err != nil {
		b.app.Logger().Warn("telegram detail send failed", "error", err)
	}
}

func renderDetailCard(ctx context.Context, scope detailScope, tab string) (string, error) {
	switch tab {
	case detailTabGit:
		return renderDetailGit(ctx, scope)
	case detailTabOverview:
		fallthrough
	default:
		return renderDetailOverview(ctx, scope)
	}
}

func resolveDetailScope(ctx context.Context, b *Bot, msg *Message, userID int64) (detailScope, error) {
	scope := detailScope{
		Context: detailContextLabel(msg),
		RunMode: "-",
		State:   "not running",
	}
	if sess, ok := currentTopicSession(ctx, b, msg); ok {
		ws, err := b.app.Store().GetWorkspace(ctx, sess.WorkspaceID)
		if err != nil {
			return scope, err
		}
		scope.Session = sess
		scope.HasSession = true
		scope.Workspace = ws
		scope.HasWorkspace = true
		scope.RunMode = detailRunMode(sess.RunnerType)
		state, err := detailSessionState(ctx, b, sess.ID)
		if err != nil {
			return scope, err
		}
		scope.State = state
		scope.Path = detailEffectivePath(sess, ws)
	} else if path := b.pinned(userID); path != "" {
		ws, err := b.app.GetSelectableWorkspaceByPath(ctx, path)
		if err != nil {
			return scope, fmt.Errorf("pinned workspace is unavailable: %w", err)
		}
		scope.Workspace = ws
		scope.HasWorkspace = true
		scope.Path = ws.Path
	}
	scope.Model, scope.ModelSource = detailResolvedModel(b, scope)
	return scope, nil
}

func detailContextLabel(msg *Message) string {
	if msg == nil {
		return "-"
	}
	if msg.Chat.IsPrivate() {
		return "private"
	}
	if msg.MessageThreadID == 0 || msg.MessageThreadID == generalTopicThreadID {
		return "group general"
	}
	return "session topic"
}

func detailRunMode(runnerType string) string {
	switch strings.TrimSpace(runnerType) {
	case "worktree", "docker":
		return runnerType
	default:
		return "local"
	}
}

func detailSessionState(ctx context.Context, b *Bot, sessionID string) (string, error) {
	active, err := b.app.ListActiveSessions(ctx)
	if err != nil {
		return "", err
	}
	for _, item := range active {
		if item.Process.SessionID != sessionID {
			continue
		}
		if item.Process.Busy {
			return "active · busy", nil
		}
		return "active · idle", nil
	}
	return "not running", nil
}

func detailEffectivePath(sess store.Session, ws store.Workspace) string {
	switch detailRunMode(sess.RunnerType) {
	case "worktree", "docker":
		if path := strings.TrimSpace(sess.WorktreePath); path != "" {
			return path
		}
		if path := strings.TrimSpace(sess.OriginalWorkspacePath); path != "" {
			return path
		}
	}
	if path := strings.TrimSpace(ws.Path); path != "" {
		return path
	}
	return strings.TrimSpace(sess.OriginalWorkspacePath)
}

func detailResolvedModel(b *Bot, scope detailScope) (string, string) {
	if scope.HasSession {
		if model := strings.TrimSpace(scope.Session.Model); model != "" {
			return model, "session"
		}
	}
	if scope.HasWorkspace {
		if model := strings.TrimSpace(scope.Workspace.Model); model != "" {
			return model, "workspace"
		}
	}
	if model := strings.TrimSpace(b.app.Config().GlobalModel); model != "" {
		return model, "global"
	}
	return "pi default", "pi default"
}

func renderDetailOverview(ctx context.Context, scope detailScope) (string, error) {
	result, err := gitops.Run(ctx, "detail_overview.sh", map[string]string{"PI_DETAIL_PATH": scope.Path})
	if err != nil {
		return "", err
	}
	gitSummary := detailValue(result.Values["GIT_SUMMARY"], "-")
	systemSummary := detailValue(result.Values["SYSTEM_SUMMARY"], "-")
	lines := []string{
		"Detail · 总览",
		"",
		"Context: " + detailValue(scope.Context, "-"),
		"Path: " + detailDisplayPath(scope.Path),
		"Run mode: " + detailValue(scope.RunMode, "-"),
		"State: " + detailValue(scope.State, "-"),
		"Model: " + detailModelLabel(scope.Model, scope.ModelSource),
		"Git: " + gitSummary,
		"System: " + systemSummary,
	}
	return strings.Join(lines, "\n"), nil
}

func renderDetailGit(ctx context.Context, scope detailScope) (string, error) {
	result, err := gitops.Run(ctx, "detail_git.sh", map[string]string{"PI_DETAIL_PATH": scope.Path})
	if err != nil {
		return "", err
	}
	report := parseDetailGitReport(result)
	if !report.Available {
		lines := []string{
			"Detail · Git",
			"",
			"Path: " + detailDisplayPath(scope.Path),
			"Git: not a git workspace",
		}
		return strings.Join(lines, "\n"), nil
	}
	lines := []string{
		"Detail · Git",
		"",
		"Repo root: " + detailDisplayPath(report.RepoRoot),
		"Path: " + detailDisplayPath(scope.Path),
		"Branch: " + detailValue(report.Branch, "-"),
		"HEAD: " + detailValue(report.Head, "-"),
		"Working tree: " + detailValue(report.WorkingTree, "0 staged · 0 unstaged · 0 untracked"),
		"Diff stat: " + detailValue(report.DiffStat, "0 files changed, +0 -0"),
	}
	if len(report.Files) == 0 {
		lines = append(lines, "", "Changed files: none")
		return strings.Join(lines, "\n"), nil
	}
	lines = append(lines, "", "Changed files:")
	for i, file := range report.Files {
		if i >= detailFileLimit {
			lines = append(lines, fmt.Sprintf("... and %d more", len(report.Files)-detailFileLimit))
			break
		}
		line := fmt.Sprintf("- %s  +%d -%d", trimMiddle(file.Path, detailPathLimit), file.Additions, file.Deletions)
		if file.Status == "untracked" {
			line += " (untracked)"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), nil
}

func parseDetailGitReport(result gitops.Result) detailGitReport {
	report := detailGitReport{
		Available:   gitops.BoolValue(result.Values, "GIT_AVAILABLE"),
		RepoRoot:    strings.TrimSpace(result.Values["REPO_ROOT"]),
		Branch:      strings.TrimSpace(result.Values["BRANCH"]),
		Head:        strings.TrimSpace(result.Values["HEAD"]),
		WorkingTree: strings.TrimSpace(result.Values["WORKING_TREE"]),
		DiffStat:    strings.TrimSpace(result.Values["DIFF_STAT"]),
	}
	for _, raw := range strings.Split(result.Stdout, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "FILE=") {
			continue
		}
		parts := strings.SplitN(strings.TrimPrefix(line, "FILE="), "\t", 4)
		if len(parts) < 3 {
			continue
		}
		additions, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		deletions, err := strconv.Atoi(parts[2])
		if err != nil {
			continue
		}
		file := detailGitFile{Path: parts[0], Additions: additions, Deletions: deletions}
		if len(parts) == 4 {
			file.Status = strings.TrimSpace(parts[3])
		}
		report.Files = append(report.Files, file)
	}
	return report
}

func detailModelLabel(model, source string) string {
	model = strings.TrimSpace(model)
	source = strings.TrimSpace(source)
	if model == "" {
		return "-"
	}
	if source == "" || source == "pi default" {
		return model
	}
	return model + " (" + source + ")"
}

func detailDisplayPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "-"
	}
	return trimMiddle(path, detailPathLimit)
}

func detailValue(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func detailKeyboard(current string) inlineKeyboardMarkup {
	return inlineKeyboardMarkup{InlineKeyboard: [][]inlineKeyboardButton{{
		{Text: detailTabButtonLabel(current, detailTabOverview, "总览"), CallbackData: "detail:" + detailTabOverview},
		{Text: detailTabButtonLabel(current, detailTabGit, "Git"), CallbackData: "detail:" + detailTabGit},
		{Text: "刷新", CallbackData: "detail:refresh:" + current},
	}}}
}

func detailTabButtonLabel(current, tab, label string) string {
	if current == tab {
		return "•" + label
	}
	return label
}

func parseDetailTab(data string) (string, bool) {
	raw := strings.TrimPrefix(data, "detail:")
	if strings.HasPrefix(raw, "refresh:") {
		raw = strings.TrimPrefix(raw, "refresh:")
	}
	switch raw {
	case detailTabOverview, detailTabGit:
		return raw, true
	default:
		return "", false
	}
}

func detailResponseTopicID(msg *Message) int {
	if msg == nil || msg.Chat.IsPrivate() {
		return 0
	}
	if msg.MessageThreadID == 0 || msg.MessageThreadID == generalTopicThreadID {
		return 0
	}
	return msg.MessageThreadID
}
