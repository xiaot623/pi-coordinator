package telegram

import (
	"context"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xiaot/pi-coordinator/internal/runner"
	"github.com/xiaot/pi-coordinator/internal/store"
)

const (
	workspacePageSize = 10
	sessionPageSize   = 8
	pinnedOnPrefix    = "Pinned on "
)

// -- UI Formatting --

func sendWorkspaces(ctx context.Context, b *Bot, chatID int64, messageID int, text, prefix string, page int) {
	total, err := b.app.Store().CountWorkspaces(ctx)
	if err != nil {
		b.send(chatID, "Failed to read workspaces: "+err.Error(), nil)
		return
	}
	if total == 0 {
		b.send(chatID, "No workspaces yet. Run /sync first.", nil)
		return
	}
	page = clampPage(page, total, workspacePageSize)
	workspaces, err := b.app.Store().ListWorkspaces(ctx, workspacePageSize, page*workspacePageSize)
	if err != nil {
		b.send(chatID, "Failed to read workspaces: "+err.Error(), nil)
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
	b.sendOrEdit(chatID, messageID, text, inlineKeyboardMarkup{InlineKeyboard: rows})
}

func sendSessions(ctx context.Context, b *Bot, chatID int64, messageID int, workspaceID int64, page int) {
	total, err := b.app.Store().CountSessions(ctx, workspaceID)
	if err != nil {
		b.send(chatID, "Failed to read sessions: "+err.Error(), nil)
		return
	}
	page = clampPage(page, total, sessionPageSize)
	sessions, err := b.app.Store().ListSessions(ctx, workspaceID, sessionPageSize, page*sessionPageSize)
	if err != nil {
		b.send(chatID, "Failed to read sessions: "+err.Error(), nil)
		return
	}
	var rows [][]inlineKeyboardButton
	rows = append(rows, inlineKeyboardRow(inlineKeyboardButton{Text: "+ New Session", CallbackData: "ns:" + strconv.FormatInt(workspaceID, 10)}))
	for _, sess := range sessions {
		rows = append(rows, inlineKeyboardRow(inlineKeyboardButton{Text: displaySession(sess), CallbackData: "s:" + sess.ID}))
	}
	rows = appendPageNav(rows, page, total, sessionPageSize, "sp:"+strconv.FormatInt(workspaceID, 10)+":")
	b.sendOrEdit(chatID, messageID, "Choose a session:", inlineKeyboardMarkup{InlineKeyboard: rows})
}

func editModelProviders(ctx context.Context, b *Bot, chatID int64, messageID int, scope string) {
	models, err := b.app.Runner().AvailableModels(ctx, false)
	if err != nil {
		b.editMessageText(chatID, messageID, "Failed to read models from pi: "+err.Error(), nil)
		return
	}
	providers := modelProviders(models)
	if len(providers) == 0 {
		b.editMessageText(chatID, messageID, "pi returned no available model providers.", nil)
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
	b.editMessageText(chatID, messageID, "Choose provider for "+scopeLabel(scope)+":", inlineKeyboardMarkup{InlineKeyboard: rows})
}

func editProviderModels(ctx context.Context, b *Bot, chatID int64, messageID int, scope, provider string) {
	models, err := b.app.Runner().AvailableModels(ctx, false)
	if err != nil {
		b.editMessageText(chatID, messageID, "Failed to read models from pi: "+err.Error(), nil)
		return
	}
	models = filterProviderModels(models, provider)
	if len(models) == 0 {
		b.editMessageText(chatID, messageID, "No models found for "+provider+".", nil)
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
	b.editMessageText(chatID, messageID, "Choose model from "+provider+":", inlineKeyboardMarkup{InlineKeyboard: rows})
}

func editModelSessions(ctx context.Context, b *Bot, chatID int64, messageID int, workspaceID int64, page int) {
	total, err := b.app.Store().CountSessions(ctx, workspaceID)
	if err != nil {
		b.editMessageText(chatID, messageID, "Failed to read sessions: "+err.Error(), nil)
		return
	}
	page = clampPage(page, total, sessionPageSize)
	sessions, err := b.app.Store().ListSessions(ctx, workspaceID, sessionPageSize, page*sessionPageSize)
	if err != nil {
		b.editMessageText(chatID, messageID, "Failed to read sessions: "+err.Error(), nil)
		return
	}
	var rows [][]inlineKeyboardButton
	for _, sess := range sessions {
		rows = append(rows, inlineKeyboardRow(inlineKeyboardButton{Text: displaySession(sess), CallbackData: "msess:" + sess.ID}))
	}
	rows = appendPageNav(rows, page, total, sessionPageSize, "msp:"+strconv.FormatInt(workspaceID, 10)+":")
	rows = append(rows, inlineKeyboardRow(inlineKeyboardButton{Text: "Cancel", CallbackData: "model:cancel"}))
	b.editMessageText(chatID, messageID, "Choose session:", inlineKeyboardMarkup{InlineKeyboard: rows})
}

// -- Helpers --

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

func sendPinnedWorkspaceMessage(b *Bot, userID, chatID int64, topicID int, ws store.Workspace) {
	messageID, err := b.sendMessage(chatID, topicID, pinnedOnPrefix+ws.Path, nil)
	if err != nil {
		b.app.Logger().Warn("telegram send pin message failed", "error", err)
		return
	}
	if err := b.pinChatMessage(chatID, messageID); err != nil {
		b.app.Logger().Warn("telegram pin message failed", "error", err)
		return
	}
	b.trackPinMessage(userID, chatID, messageID)
}

func isPinnedWorkspaceText(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), pinnedOnPrefix)
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
	return topicTitleWithDate(prompt, time.Now())
}

func topicTitleWithDate(prompt string, now time.Time) string {
	line := strings.TrimSpace(strings.Split(prompt, "\n")[0])
	if line == "" {
		line = "New task"
	}
	suffix := " " + now.Format("20060102")
	runes := []rune(line)
	maxTitleRunes := 128 - len([]rune(suffix))
	if len(runes) > maxTitleRunes {
		line = string(runes[:maxTitleRunes])
	}
	return line + suffix
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

func createdTopicKeyboard(workspaceID int64, pinned bool, groupChatID int64, topicID int) inlineKeyboardMarkup {
	markup := taskKeyboard(workspaceID, pinned)
	markup.InlineKeyboard = append(
		[][]inlineKeyboardButton{inlineKeyboardRow(inlineKeyboardButton{Text: "Follow up", URL: topicURL(groupChatID, topicID)})},
		markup.InlineKeyboard...,
	)
	return markup
}

func topicURL(groupChatID int64, topicID int) string {
	chatID := strconv.FormatInt(groupChatID, 10)
	chatID = strings.TrimPrefix(chatID, "-")
	chatID = strings.TrimPrefix(chatID, "100")
	return "https://t.me/c/" + chatID + "/" + strconv.Itoa(topicID)
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
	Name  float64
	Ctx   float64
	Out   float64
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
	padCount := widths.Icons - len(icons)
	for i := 0; i < padCount; i++ {
		iconStr += "\u3000" // U+3000
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
	if prefix == "pinws:" {
		return "pinwsp:"
	}
	if prefix == "mws:" {
		return "mwp:"
	}
	if prefix == "msws:" {
		return "mswp:"
	}
	return "wp:"
}

// -- Keyboard Structs --

type inlineKeyboardMarkup struct {
	InlineKeyboard [][]inlineKeyboardButton `json:"inline_keyboard"`
}

type inlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
	URL          string `json:"url,omitempty"`
}

func inlineKeyboardRow(buttons ...inlineKeyboardButton) []inlineKeyboardButton {
	return buttons
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
