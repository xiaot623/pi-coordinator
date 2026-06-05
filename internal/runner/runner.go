package runner

import "context"

// Runner is the coordinator-facing interface for driving pi.
//
// Implementations can be local processes, remote agents, or any future runtime
// that accepts prompts and steering messages for a session.
type Runner interface {
	Prompt(ctx context.Context, req StartRequest, message string) error
	Steer(ctx context.Context, req StartRequest, message string) error
	AvailableModels(ctx context.Context, refresh bool) ([]ModelInfo, error)
}

type StartRequest struct {
	SessionID            string
	Title                string
	Workspace            string
	TopicID              int
	Model                string
	Existing             bool
	Role                 string
	TraceTelegramToken   string
	TraceTelegramChatIDs []int64
}

type ModelInfo struct {
	Provider      string
	ID            string
	Name          string
	ContextWindow int64
	MaxTokens     int64
	Reasoning     bool
	Inputs        []string
}
