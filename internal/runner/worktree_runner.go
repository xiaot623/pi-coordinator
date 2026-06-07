package runner

import (
	"context"
	"log/slog"
	"time"
)

type WorktreeOptions struct {
	LocalOptions
	SessionDir string
	Logger     *slog.Logger
}

type WorktreeRunner struct {
	local *Local
}

func NewWorktreeRunner(opts WorktreeOptions) *WorktreeRunner {
	localOpts := opts.LocalOptions
	if opts.SessionDir != "" {
		localOpts.SessionDir = opts.SessionDir
	}
	if opts.Logger != nil {
		localOpts.Logger = opts.Logger
	}
	if localOpts.IdleTimeout == 0 {
		localOpts.IdleTimeout = 5 * time.Minute
	}
	return &WorktreeRunner{local: NewLocal(localOpts)}
}

func (r *WorktreeRunner) Prompt(ctx context.Context, req StartRequest, message string, images []ImageAttachment) error {
	return r.local.Prompt(ctx, req, message, images)
}

func (r *WorktreeRunner) Steer(ctx context.Context, req StartRequest, message string, images []ImageAttachment) error {
	return r.local.Steer(ctx, req, message, images)
}

func (r *WorktreeRunner) AvailableModels(ctx context.Context, refresh bool) ([]ModelInfo, error) {
	return r.local.AvailableModels(ctx, refresh)
}
