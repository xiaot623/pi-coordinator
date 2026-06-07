package telegram

import (
	"context"
	"strings"
)

type HandlerFunc func(ctx context.Context, b *Bot, update Update)

type Router struct {
	bot              *Bot
	commandHandlers  map[string]HandlerFunc
	callbackHandlers map[string]HandlerFunc // Prefix match for callbacks
	textHandler      HandlerFunc            // Fallback for text messages
	topicHandler     HandlerFunc            // Handler for topic messages in the group
}

const generalTopicThreadID = 1

func NewRouter(bot *Bot) *Router {
	return &Router{
		bot:              bot,
		commandHandlers:  make(map[string]HandlerFunc),
		callbackHandlers: make(map[string]HandlerFunc),
	}
}

// Command registers a handler for a specific command (e.g., "start", without the slash).
func (r *Router) Command(cmd string, handler HandlerFunc) {
	r.commandHandlers[cmd] = handler
}

// Callback registers a handler for a callback query prefix.
func (r *Router) Callback(prefix string, handler HandlerFunc) {
	r.callbackHandlers[prefix] = handler
}

// Text registers a handler for private text messages that are not commands.
func (r *Router) Text(handler HandlerFunc) {
	r.textHandler = handler
}

// Topic registers a handler for messages in the group chat.
func (r *Router) Topic(handler HandlerFunc) {
	r.topicHandler = handler
}

func (r *Router) handleCommand(ctx context.Context, update Update) {
	msg := update.Message
	cmd := msg.Command()
	if handler, ok := r.commandHandlers[cmd]; ok {
		handler(ctx, r.bot, update)
	} else {
		r.bot.send(msg.Chat.ID, "Unknown command. Available: /help /workspace /add /new /status /open /rebase /commit /sync /pin /unpin /model /bots", nil)
	}
}

// HandleUpdate routes the incoming update to the appropriate handler.
func (r *Router) HandleUpdate(ctx context.Context, update Update) {
	if update.ManagedBot != nil {
		r.bot.handleManagedBotUpdated(ctx, update.ManagedBot)
		return
	}

	if update.CallbackQuery != nil {
		if update.CallbackQuery.From == nil || !r.bot.allowed(update.CallbackQuery.From.ID) {
			return
		}
		r.bot.answerCallback(update.CallbackQuery.ID, "")

		data := update.CallbackQuery.Data
		// Find longest matching prefix
		var bestMatch string
		var bestHandler HandlerFunc

		for prefix, handler := range r.callbackHandlers {
			if strings.HasPrefix(data, prefix) {
				if len(prefix) > len(bestMatch) {
					bestMatch = prefix
					bestHandler = handler
				}
			}
		}

		if bestHandler != nil {
			bestHandler(ctx, r.bot, update)
		}
		return
	}

	msg := update.Message
	if msg == nil || msg.From == nil || msg.From.IsBot {
		return
	}
	if !r.bot.allowed(msg.From.ID) {
		return
	}

	if msg.ManagedBotCreated != nil {
		r.bot.handleManagedBotCreated(ctx, msg.Chat.ID, msg.ManagedBotCreated)
		return
	}

	if msg.Chat.IsPrivate() {
		if msg.IsCommand() {
			r.handleCommand(ctx, update)
			return
		}
		if r.textHandler != nil && hasContent(msg) {
			r.textHandler(ctx, r.bot, update)
		}
		return
	}

	if msg.Chat.ID != r.bot.app.Config().Telegram.GroupChatID || !hasContent(msg) {
		return
	}

	if msg.IsCommand() && (msg.MessageThreadID == 0 || msg.MessageThreadID == generalTopicThreadID) {
		r.handleCommand(ctx, update)
		return
	}

	if msg.MessageThreadID == 0 || msg.MessageThreadID == generalTopicThreadID {
		if r.textHandler == nil {
			return
		}
		if r.bot.pinned(msg.From.ID) != "" {
			r.textHandler(ctx, r.bot, update)
			return
		}
		if msg.ReplyToMessage != nil && r.bot.hasPendingPromptReply(msg.From.ID, msg.Chat.ID, msg.ReplyToMessage.MessageID) {
			r.textHandler(ctx, r.bot, update)
		}
		return
	}

	// Session topic message in the configured group chat.
	if msg.MessageThreadID != 0 {
		if msg.IsCommand() {
			if _, ok := r.commandHandlers[msg.Command()]; ok {
				r.handleCommand(ctx, update)
				return
			}
		}
		if r.topicHandler != nil {
			r.topicHandler(ctx, r.bot, update)
		}
	}
}
