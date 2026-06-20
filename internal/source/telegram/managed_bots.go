package telegram

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	genericRole = "Generic"
	deliverRole = "Deliver"
)

type managedBotRecord struct {
	Role               string `json:"role"`
	Name               string `json:"name"`
	Username           string `json:"username"`
	UserID             int64  `json:"user_id,omitempty"`
	Token              string `json:"token,omitempty"`
	Status             string `json:"status"`
	RequestID          int    `json:"request_id,omitempty"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
	LastTokenUpdatedAt string `json:"last_token_updated_at,omitempty"`
}

type managedBotFile struct {
	Bots []managedBotRecord `json:"bots"`
}

type managedBotCredentials struct {
	Role    string
	Token   string
	ChatIDs []int64
}

func managedBotRoles() []string {
	return []string{genericRole, deliverRole}
}

func (b *Bot) ensureTraceBot(ctx context.Context, role string, chatIDForNotice int64) (*managedBotCredentials, error) {
	record, err := b.ensureManagedBotRecord(ctx, role)
	if err != nil {
		return nil, err
	}
	if record.Token == "" {
		refreshed, err := b.refreshManagedBotTokenByUsername(ctx, record)
		if err == nil && refreshed.Token != "" {
			record = refreshed
		}
	}
	if record.Token == "" {
		b.sendManagedBotRequest(ctx, chatIDForNotice, record)
		return nil, fmt.Errorf("managed bot for role %s is pending creation", record.Role)
	}
	if err := b.ensureManagedBotInGroup(ctx, record); err != nil {
		b.sendManagedBotAddToGroup(chatIDForNotice, record)
		return nil, err
	}
	return b.credentialsForManagedBot(record), nil
}

func (b *Bot) managedBotCredentialsIfReady(ctx context.Context, role string) (*managedBotCredentials, bool) {
	role = normalizeRole(role)
	file, err := readManagedBotFile(b.managedBotsPath())
	if err != nil {
		b.app.Logger().Warn("read managed bot file failed", "role", role, "error", err)
		return nil, false
	}
	idx := findManagedBotByRole(file.Bots, role)
	if idx < 0 {
		return nil, false
	}
	record := file.Bots[idx]
	if record.Token == "" {
		refreshed, err := b.refreshManagedBotTokenByUsername(ctx, record)
		if err != nil || refreshed.Token == "" {
			if err != nil {
				b.app.Logger().Debug("refresh managed bot token failed", "role", role, "error", err)
			}
			return nil, false
		}
		record = refreshed
	}
	if err := b.ensureManagedBotInGroup(ctx, record); err != nil {
		b.app.Logger().Debug("managed bot is not ready in group", "role", role, "error", err)
		return nil, false
	}
	return b.credentialsForManagedBot(record), true
}

func (b *Bot) credentialsForManagedBot(record managedBotRecord) *managedBotCredentials {
	return &managedBotCredentials{
		Role:    record.Role,
		Token:   record.Token,
		ChatIDs: []int64{b.app.Config().Telegram.GroupChatID},
	}
}

func (b *Bot) ensureManagedBotRecord(ctx context.Context, role string) (managedBotRecord, error) {
	role = normalizeRole(role)
	path := b.managedBotsPath()
	file, err := readManagedBotFile(path)
	if err != nil {
		return managedBotRecord{}, err
	}
	if idx := findManagedBotByRole(file.Bots, role); idx >= 0 {
		return file.Bots[idx], nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	record := managedBotRecord{
		Role:      role,
		Name:      role,
		Username:  makeManagedBotUsername(role),
		Status:    "pending_creation",
		RequestID: randomRequestID(),
		CreatedAt: now,
		UpdatedAt: now,
	}
	file.Bots = append(file.Bots, record)
	if err := writeManagedBotFile(path, file); err != nil {
		return managedBotRecord{}, err
	}
	return record, nil
}

func (b *Bot) handleManagedBotCreated(ctx context.Context, chatID int64, created *ManagedBotCreated) {
	if created == nil || created.Bot == nil {
		return
	}
	if err := b.saveManagedBotToken(ctx, created.Bot); err != nil {
		b.send(chatID, "Managed bot created, but token fetch failed: "+err.Error(), nil)
		return
	}
	b.sendManagedBotAddToGroup(chatID, b.managedBotRecordByUser(created.Bot.ID))
}

func (b *Bot) handleManagedBotUpdated(ctx context.Context, updated *ManagedBotUpdated) {
	if updated == nil {
		return
	}
	bot := updated.Bot
	if bot == nil {
		bot = updated.User
	}
	if bot == nil {
		return
	}
	if err := b.saveManagedBotToken(ctx, bot); err != nil {
		b.app.Logger().Warn("managed bot token update failed", "bot_id", bot.ID, "error", err)
	}
}

func (b *Bot) refreshManagedBotTokenByUsername(ctx context.Context, record managedBotRecord) (managedBotRecord, error) {
	if record.UserID == 0 {
		userID, err := b.getManagedBotUserIDByUsername(ctx, record.Username)
		if err != nil {
			return record, err
		}
		record.UserID = userID
	}
	if err := b.saveManagedBotToken(ctx, &User{ID: record.UserID, IsBot: true, Username: record.Username}); err != nil {
		return record, err
	}
	file, err := readManagedBotFile(b.managedBotsPath())
	if err != nil {
		return record, err
	}
	if idx := findManagedBotByRole(file.Bots, record.Role); idx >= 0 {
		return file.Bots[idx], nil
	}
	return record, nil
}

func (b *Bot) getManagedBotUserIDByUsername(ctx context.Context, username string) (int64, error) {
	username = strings.TrimPrefix(username, "@")
	if username == "" {
		return 0, errors.New("managed bot username is empty")
	}
	var resp struct {
		OK          bool   `json:"ok"`
		Result      Chat   `json:"result"`
		Description string `json:"description"`
	}
	if err := b.callTelegramCtx(ctx, "getChat", map[string]any{"chat_id": "@" + username}, &resp, 20*time.Second); err != nil {
		return 0, err
	}
	if !resp.OK {
		return 0, errors.New(resp.Description)
	}
	if resp.Result.ID == 0 {
		return 0, errors.New("managed bot id is unavailable")
	}
	return resp.Result.ID, nil
}

func (b *Bot) saveManagedBotToken(ctx context.Context, bot *User) error {
	token, err := b.getManagedBotToken(ctx, bot.ID)
	if err != nil {
		return err
	}
	path := b.managedBotsPath()
	file, err := readManagedBotFile(path)
	if err != nil {
		return err
	}
	idx := findManagedBotByUsername(file.Bots, bot.Username)
	if idx < 0 {
		idx = findManagedBotByUserID(file.Bots, bot.ID)
	}
	if idx < 0 {
		role := inferRoleFromManagedUsername(bot.Username)
		now := time.Now().UTC().Format(time.RFC3339)
		file.Bots = append(file.Bots, managedBotRecord{
			Role:      role,
			Name:      role,
			Username:  bot.Username,
			CreatedAt: now,
		})
		idx = len(file.Bots) - 1
	}
	now := time.Now().UTC().Format(time.RFC3339)
	file.Bots[idx].UserID = bot.ID
	file.Bots[idx].Token = token
	file.Bots[idx].Status = "token_ready"
	file.Bots[idx].UpdatedAt = now
	file.Bots[idx].LastTokenUpdatedAt = now
	if file.Bots[idx].Username == "" {
		file.Bots[idx].Username = bot.Username
	}
	return writeManagedBotFile(path, file)
}

func (b *Bot) ensureManagedBotInGroup(ctx context.Context, record managedBotRecord) error {
	var resp struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := b.callTelegramWithTokenCtx(ctx, record.Token, "getChat", map[string]any{
		"chat_id": b.app.Config().Telegram.GroupChatID,
	}, &resp, 20*time.Second); err != nil {
		return fmt.Errorf("managed bot @%s is not in the configured group yet", record.Username)
	}
	if !resp.OK {
		return errors.New(resp.Description)
	}
	b.ensureManagedBotPermissions(record)
	return nil
}

func (b *Bot) ensureManagedBotPermissions(record managedBotRecord) {
	if record.UserID == 0 {
		return
	}
	var resp telegramOK
	err := b.callTelegram("promoteChatMember", map[string]any{
		"chat_id":           b.app.Config().Telegram.GroupChatID,
		"user_id":           record.UserID,
		"can_manage_chat":   true,
		"can_manage_topics": true,
		"can_pin_messages":  true,
	}, &resp)
	if err != nil || !resp.OK {
		if err == nil {
			err = errors.New(resp.Description)
		}
		b.app.Logger().Warn("managed bot permission setup failed", "chat_id", b.app.Config().Telegram.GroupChatID, "bot_id", record.UserID, "error", err)
	}
}

func (b *Bot) sendManagedBotRequest(ctx context.Context, chatID int64, record managedBotRecord) {
	b.sendManagedBotSetup(ctx, chatID, record)
}

func (b *Bot) sendManagedBotAddToGroup(chatID int64, record managedBotRecord) {
	b.sendManagedBotSetup(context.Background(), chatID, record)
}

func (b *Bot) sendManagedBotSetup(ctx context.Context, chatID int64, record managedBotRecord) {
	if record.Username == "" || chatID == 0 {
		return
	}
	manager, err := b.getMe(ctx)
	if err != nil {
		b.send(chatID, "Managed bot setup is pending, but manager bot lookup failed: "+err.Error(), traceBotSetupKeyboard("", record))
		return
	}
	if !manager.CanManageBots {
		b.send(chatID, fmt.Sprintf(
			"Managed bot record created for role %s as @%s, but the coordinator bot cannot manage bots yet. Enable managed bot control for @%s in Telegram, then run /new again.",
			record.Role,
			record.Username,
			manager.Username,
		), traceBotSetupKeyboard("", record))
		return
	}
	if manager.Username == "" {
		b.send(chatID, fmt.Sprintf("Managed bot record created for role %s as @%s, but the coordinator bot has no username for the creation link.", record.Role, record.Username), traceBotSetupKeyboard("", record))
		return
	}
	link := managedBotCreateLink(manager.Username, record)
	text := fmt.Sprintf(
		"Set up trace bot for role %s.\n\n1. Create @%s.\n2. Add it to the configured group.\n3. Tap Retry to continue the pending task.",
		record.Role,
		record.Username,
	)
	b.send(chatID, text, traceBotSetupKeyboard(link, record))
}

func traceBotSetupKeyboard(createLink string, record managedBotRecord) inlineKeyboardMarkup {
	addLink := "https://t.me/" + record.Username + "?startgroup=trace"
	var rows [][]inlineKeyboardButton
	if createLink != "" {
		rows = append(rows, []inlineKeyboardButton{{Text: "Create bot", URL: createLink}})
	}
	rows = append(rows, []inlineKeyboardButton{{Text: "Add to group", URL: addLink}})
	rows = append(rows, []inlineKeyboardButton{{Text: "Retry", CallbackData: "trace:retry"}})
	return inlineKeyboardMarkup{InlineKeyboard: rows}
}

func (b *Bot) listManagedBots(chatID int64) {
	file, err := readManagedBotFile(b.managedBotsPath())
	if err != nil {
		b.send(chatID, "Failed to read managed bots: "+err.Error(), nil)
		return
	}
	if len(file.Bots) == 0 {
		b.send(chatID, "No managed bots yet. The Generic bot will be requested on first task start.", nil)
		return
	}
	lines := []string{"Managed bots:"}
	for _, bot := range file.Bots {
		label := bot.Role + " -> @" + bot.Username + " (" + bot.Status + ")"
		if bot.Token != "" {
			label += " token:ready"
		}
		lines = append(lines, label)
	}
	b.send(chatID, strings.Join(lines, "\n"), nil)
}

func (b *Bot) sendNewBotRoleKeyboard(chatID int64) {
	var rows [][]inlineKeyboardButton
	for _, role := range managedBotRoles() {
		rows = append(rows, inlineKeyboardRow(inlineKeyboardButton{Text: role, CallbackData: "newbot:" + role}))
	}
	b.send(chatID, "Choose a managed bot role:", inlineKeyboardMarkup{InlineKeyboard: rows})
}

func (b *Bot) handleNewBotRole(ctx context.Context, chatID int64, role string) {
	role = normalizeRole(role)
	if !isKnownManagedBotRole(role) {
		b.send(chatID, "Unknown managed bot role: "+role, nil)
		return
	}
	record, err := b.ensureManagedBotRecord(ctx, role)
	if err != nil {
		b.send(chatID, "Failed to read managed bot config: "+err.Error(), nil)
		return
	}
	if record.Token == "" {
		refreshed, err := b.refreshManagedBotTokenByUsername(ctx, record)
		if err == nil && refreshed.Token != "" {
			record = refreshed
		}
	}
	if record.Token != "" {
		b.send(chatID, fmt.Sprintf("Existing %s bot token:\n%s", record.Role, record.Token), nil)
		return
	}
	b.sendManagedBotSetup(ctx, chatID, record)
}

func isKnownManagedBotRole(role string) bool {
	role = normalizeRole(role)
	for _, candidate := range managedBotRoles() {
		if normalizeRole(candidate) == role {
			return true
		}
	}
	return false
}

func (b *Bot) managedBotRecordByUser(userID int64) managedBotRecord {
	file, err := readManagedBotFile(b.managedBotsPath())
	if err != nil {
		return managedBotRecord{}
	}
	if idx := findManagedBotByUserID(file.Bots, userID); idx >= 0 {
		return file.Bots[idx]
	}
	return managedBotRecord{}
}

func (b *Bot) managedBotsPath() string {
	return filepath.Join(b.app.Paths().DataDir, "telegram-managed-bots.json")
}

func (b *Bot) getMe(ctx context.Context) (*User, error) {
	var resp struct {
		OK          bool   `json:"ok"`
		Result      User   `json:"result"`
		Description string `json:"description"`
	}
	if err := b.callTelegramCtx(ctx, "getMe", map[string]any{}, &resp, 20*time.Second); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, errors.New(resp.Description)
	}
	return &resp.Result, nil
}

func (b *Bot) getManagedBotToken(ctx context.Context, userID int64) (string, error) {
	var resp struct {
		OK          bool   `json:"ok"`
		Result      string `json:"result"`
		Description string `json:"description"`
	}
	if err := b.callTelegramCtx(ctx, "getManagedBotToken", map[string]any{"user_id": userID}, &resp, 20*time.Second); err != nil {
		return "", err
	}
	if !resp.OK {
		return "", errors.New(resp.Description)
	}
	return resp.Result, nil
}

func managedBotRequestMarkup(record managedBotRecord) any {
	return map[string]any{
		"keyboard": [][]map[string]any{{
			{
				"text": "Create " + record.Role + " bot",
				"request_managed_bot": map[string]any{
					"request_id":         record.RequestID,
					"suggested_name":     record.Name,
					"suggested_username": record.Username,
				},
			},
		}},
		"one_time_keyboard": true,
		"resize_keyboard":   true,
	}
}

func managedBotCreateLink(managerUsername string, record managedBotRecord) string {
	u := "https://t.me/newbot/" + managerUsername + "/" + record.Username
	if record.Name != "" {
		u += "?name=" + url.QueryEscape(record.Name)
	}
	return u
}

func readManagedBotFile(path string) (managedBotFile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return managedBotFile{}, nil
	}
	if err != nil {
		return managedBotFile{}, err
	}
	var file managedBotFile
	if err := json.Unmarshal(data, &file); err != nil {
		return managedBotFile{}, err
	}
	return file, nil
}

func writeManagedBotFile(path string, file managedBotFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func findManagedBotByRole(bots []managedBotRecord, role string) int {
	role = normalizeRole(role)
	for i, bot := range bots {
		if normalizeRole(bot.Role) == role {
			return i
		}
	}
	return -1
}

func findManagedBotByUsername(bots []managedBotRecord, username string) int {
	username = strings.TrimPrefix(strings.ToLower(username), "@")
	for i, bot := range bots {
		if strings.ToLower(bot.Username) == username {
			return i
		}
	}
	return -1
}

func findManagedBotByUserID(bots []managedBotRecord, userID int64) int {
	for i, bot := range bots {
		if bot.UserID == userID && userID != 0 {
			return i
		}
	}
	return -1
}

func normalizeRole(role string) string {
	role = strings.TrimSpace(role)
	if role == "" {
		return genericRole
	}
	runes := []rune(role)
	return strings.ToUpper(string(runes[0])) + string(runes[1:])
}

func makeManagedBotUsername(role string) string {
	prefix := strings.ToLower(regexp.MustCompile(`[^a-zA-Z]+`).ReplaceAllString(role, ""))
	if prefix == "" {
		prefix = "agent"
	}
	for len(prefix) < 4 {
		prefix += "x"
	}
	return prefix + randomSuffix(4) + "_bot"
}

func inferRoleFromManagedUsername(username string) string {
	username = strings.TrimSuffix(strings.TrimPrefix(strings.ToLower(username), "@"), "_bot")
	username = regexp.MustCompile(`[0-9a-z]+$`).ReplaceAllString(username, "")
	if username == "" {
		return genericRole
	}
	runes := []rune(username)
	return strings.ToUpper(string(runes[0])) + string(runes[1:])
}

func randomSuffix(length int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var out strings.Builder
	for out.Len() < length {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			out.WriteString(strconv.FormatInt(time.Now().UnixNano(), 36))
			break
		}
		out.WriteByte(alphabet[n.Int64()])
	}
	result := out.String()
	if len(result) > length {
		return result[:length]
	}
	return result
}

func randomRequestID() int {
	n, err := rand.Int(rand.Reader, big.NewInt(1<<30))
	if err != nil {
		return int(time.Now().Unix() & 0x3fffffff)
	}
	return int(n.Int64()) + 1
}
