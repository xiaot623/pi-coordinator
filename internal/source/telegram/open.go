package telegram

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/xiaot/pi-coordinator/internal/store"
)

func sessionOpenPath(sess store.Session, ws store.Workspace) (path string, label string, err error) {
	switch strings.TrimSpace(sess.RunnerType) {
	case "", "local":
		if ws.Path == "" {
			return "", "", fmt.Errorf("workspace path is empty")
		}
		return ws.Path, "workspace", nil
	case "worktree", "docker":
		if sess.WorktreePath == "" {
			return "", "", fmt.Errorf("%s session has no worktree path", sess.RunnerType)
		}
		return sess.WorktreePath, "worktree", nil
	default:
		if sess.WorktreePath != "" {
			return sess.WorktreePath, "worktree", nil
		}
		if ws.Path == "" {
			return "", "", fmt.Errorf("workspace path is empty")
		}
		return ws.Path, "workspace", nil
	}
}

func openWorkspace(ctx context.Context, tool, workspacePath string) error {
	tool = strings.TrimSpace(tool)
	if tool == "" {
		tool = "iterm2"
	}
	if strings.EqualFold(tool, "iterm2") || strings.EqualFold(tool, "iterm") {
		return openWorkspaceInITerm2(ctx, workspacePath)
	}
	cmd := exec.CommandContext(ctx, tool, workspacePath)
	return cmd.Run()
}

func openWorkspaceInITerm2(ctx context.Context, workspacePath string) error {
	if workspacePath == "" {
		return fmt.Errorf("workspace path is empty")
	}
	cdCommand := "cd " + shellQuote(workspacePath)
	script := strings.Join([]string{
		`tell application "iTerm2"`,
		`activate`,
		`create window with default profile`,
		`tell current session of current window`,
		`write text ` + strconv.Quote(cdCommand),
		`end tell`,
		`end tell`,
	}, "\n")
	return exec.CommandContext(ctx, "osascript", "-e", script).Run()
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
