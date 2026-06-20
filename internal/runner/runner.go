package runner

import (
	"context"
	"errors"
	"time"
)

// ImageAttachment mirrors pi's RPC image format.
type ImageAttachment struct {
	Type     string `json:"type"`     // "image"
	Data     string `json:"data"`     // base64-encoded
	MimeType string `json:"mimeType"` // e.g. "image/jpeg"
}

// Runner is the coordinator-facing interface for driving pi.
//
// Implementations can be local processes, remote agents, or any future runtime
// that accepts prompts and steering messages for a session.
type Runner interface {
	Prompt(ctx context.Context, req StartRequest, message string, images []ImageAttachment) error
	Steer(ctx context.Context, req StartRequest, message string, images []ImageAttachment) error
	AvailableModels(ctx context.Context, refresh bool) ([]ModelInfo, error)
	ActiveProcesses() []ProcessInfo
	StopSession(ctx context.Context, sessionID string) error
}

// ProcessInfo describes a currently active pi runtime process for a session.
type ProcessInfo struct {
	SessionID string
	PID       int
	Busy      bool
	StartedAt time.Time
	LastUsed  time.Time
}

var ErrSessionNotActive = errors.New("session is not active")

type StartRequest struct {
	SessionID            string
	Title                string
	Workspace            string
	TopicID              int
	Model                string
	Existing             bool
	Role                 string
	Env                  map[string]string
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
