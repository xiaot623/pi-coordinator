package runner

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type WorktreeMeta struct {
	Path       string
	Branch     string
	BaseCommit string
}

type WorktreeManager struct {
	Root string
}

func NewWorktreeManager(root string) *WorktreeManager {
	return &WorktreeManager{Root: root}
}

func IsGitWorkspace(ctx context.Context, workspace string) bool {
	_, err := gitOutput(ctx, workspace, "rev-parse", "--show-toplevel")
	if err != nil {
		return false
	}
	_, err = gitOutput(ctx, workspace, "rev-parse", "--verify", "HEAD")
	return err == nil
}

func HasDirtyChanges(ctx context.Context, workspace string) bool {
	out, err := gitOutput(ctx, workspace, "status", "--short")
	return err == nil && strings.TrimSpace(out) != ""
}

func (m *WorktreeManager) Ensure(ctx context.Context, sessionID, workspace string, existing WorktreeMeta) (WorktreeMeta, bool, error) {
	if existing.Path != "" {
		if st, err := os.Stat(existing.Path); err == nil && st.IsDir() {
			return existing, false, nil
		}
	}
	if m.Root == "" {
		return WorktreeMeta{}, false, errors.New("worktree root is required")
	}
	if !IsGitWorkspace(ctx, workspace) {
		return WorktreeMeta{}, false, errors.New("worktree mode requires a Git workspace with at least one commit")
	}
	baseCommit, err := gitOutput(ctx, workspace, "rev-parse", "HEAD")
	if err != nil {
		return WorktreeMeta{}, false, err
	}
	repoRoot, err := gitOutput(ctx, workspace, "rev-parse", "--show-toplevel")
	if err != nil {
		return WorktreeMeta{}, false, err
	}
	repoName := filepath.Base(strings.TrimSpace(repoRoot))
	suffix := existing.Branch
	if !isRandomSuffix(suffix) {
		suffix, err = randomWorktreeSuffix(ctx, workspace, m.Root, repoName)
		if err != nil {
			return WorktreeMeta{}, false, err
		}
	}
	meta := WorktreeMeta{
		Path:       filepath.Join(m.Root, repoName+"_"+suffix),
		Branch:     suffix,
		BaseCommit: strings.TrimSpace(baseCommit),
	}
	if err := os.MkdirAll(filepath.Dir(meta.Path), 0o755); err != nil {
		return WorktreeMeta{}, false, err
	}
	if _, err := os.Stat(meta.Path); err == nil {
		return meta, true, nil
	}
	cmd := exec.CommandContext(ctx, "git", "-C", workspace, "worktree", "add", "-b", meta.Branch, meta.Path, meta.BaseCommit)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return WorktreeMeta{}, false, fmt.Errorf("create git worktree: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return meta, true, nil
}

func randomWorktreeSuffix(ctx context.Context, workspace, root, repoName string) (string, error) {
	for range 20 {
		suffix, err := randomAlnum(10)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(filepath.Join(root, repoName+"_"+suffix)); err == nil {
			continue
		}
		if _, err := gitOutput(ctx, workspace, "rev-parse", "--verify", "--quiet", "refs/heads/"+suffix); err != nil {
			return suffix, nil
		}
	}
	return "", errors.New("generate unique worktree suffix")
}

func isRandomSuffix(value string) bool {
	if len(value) != 10 {
		return false
	}
	for _, r := range value {
		if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			continue
		}
		return false
	}
	return true
}

func randomAlnum(length int) (string, error) {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	var b strings.Builder
	b.Grow(length)
	max := big.NewInt(int64(len(alphabet)))
	for b.Len() < length {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b.WriteByte(alphabet[n.Int64()])
	}
	return b.String(), nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmdArgs := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", err
	}
	return string(output), nil
}
