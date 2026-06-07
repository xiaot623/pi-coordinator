package app

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xiaot/pi-coordinator/internal/runner"
	"github.com/xiaot/pi-coordinator/internal/store"
)

type ActiveSession struct {
	Session    store.Session
	Workspace  store.Workspace
	Process    runner.ProcessInfo
	RunnerType string
}

func (a *App) ListActiveSessions(ctx context.Context) ([]ActiveSession, error) {
	var out []ActiveSession
	for _, item := range []struct {
		runnerType string
		runner     runner.Runner
	}{
		{runnerType: "local", runner: a.local},
		{runnerType: "worktree", runner: a.work},
		{runnerType: "docker", runner: a.docker},
	} {
		for _, proc := range item.runner.ActiveProcesses() {
			active := ActiveSession{Process: proc, RunnerType: item.runnerType}
			sess, err := a.store.GetSession(ctx, proc.SessionID)
			if err == nil {
				active.Session = sess
				if ws, err := a.store.GetWorkspace(ctx, sess.WorkspaceID); err == nil {
					active.Workspace = ws
				}
				if normalized := normalizedRunnerType(sess.RunnerType); normalized != "" {
					active.RunnerType = normalized
				}
			}
			out = append(out, active)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Process.Busy != out[j].Process.Busy {
			return out[i].Process.Busy
		}
		if !out[i].Process.LastUsed.Equal(out[j].Process.LastUsed) {
			return out[i].Process.LastUsed.After(out[j].Process.LastUsed)
		}
		return strings.Compare(out[i].Process.SessionID, out[j].Process.SessionID) < 0
	})
	return out, nil
}

func (a *App) StopActiveSession(ctx context.Context, sessionID string) error {
	if sess, err := a.store.GetSession(ctx, sessionID); err == nil {
		switch normalizedRunnerType(sess.RunnerType) {
		case "worktree":
			return a.work.StopSession(ctx, sessionID)
		case "docker":
			return a.docker.StopSession(ctx, sessionID)
		default:
			return a.local.StopSession(ctx, sessionID)
		}
	}
	var firstErr error
	for _, r := range []runner.Runner{a.local, a.work, a.docker} {
		if err := r.StopSession(ctx, sessionID); err == nil {
			return nil
		} else if err != runner.ErrSessionNotActive && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return firstErr
	}
	return runner.ErrSessionNotActive
}

func FormatRelativeDuration(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return pluralDuration(int(d/time.Minute), "min")
	case d < 24*time.Hour:
		h := int(d / time.Hour)
		m := int((d % time.Hour) / time.Minute)
		if m == 0 {
			return pluralDuration(h, "hr")
		}
		return pluralDuration(h, "hr") + " " + pluralDuration(m, "min")
	default:
		return pluralDuration(int(d/(24*time.Hour)), "day")
	}
}

func pluralDuration(v int, unit string) string {
	if unit == "day" && v > 1 {
		return strings.TrimSpace(strings.Join([]string{strconv.Itoa(v), unit + "s"}, " "))
	}
	return strings.TrimSpace(strings.Join([]string{strconv.Itoa(v), unit}, " "))
}
