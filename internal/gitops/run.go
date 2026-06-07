package gitops

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

//go:embed scripts/*.sh
var scriptFS embed.FS

type Result struct {
	Values map[string]string
	Stdout string
	Stderr string
}

type RunError struct {
	Script string
	Stdout string
	Stderr string
	Err    error
}

func (e *RunError) Error() string {
	if msg := strings.TrimSpace(e.Stderr); msg != "" {
		return msg
	}
	if msg := strings.TrimSpace(e.Stdout); msg != "" {
		return msg
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Script + " failed"
}

func (e *RunError) Unwrap() error { return e.Err }

func Run(ctx context.Context, script string, env map[string]string) (Result, error) {
	content, err := scriptFS.ReadFile("scripts/" + script)
	if err != nil {
		return Result{}, err
	}
	tmp, err := os.CreateTemp("", "pi-gitops-*.sh")
	if err != nil {
		return Result{}, err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return Result{}, err
	}
	if err := tmp.Close(); err != nil {
		return Result{}, err
	}

	cmd := exec.CommandContext(ctx, "bash", path)
	cmd.Env = append(os.Environ(), envList(env)...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return Result{}, &RunError{
			Script: script,
			Stdout: stdout.String(),
			Stderr: stderr.String(),
			Err:    err,
		}
	}
	return Result{Values: parseOutput(stdout.String()), Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

func envList(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out
}

func parseOutput(stdout string) map[string]string {
	values := make(map[string]string)
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		values[key] = strings.TrimSpace(value)
	}
	return values
}

func RequireValue(values map[string]string, key string) (string, error) {
	value := strings.TrimSpace(values[key])
	if value == "" {
		return "", errors.New("missing script output: " + key)
	}
	return value, nil
}

func BoolValue(values map[string]string, key string) bool {
	value := strings.TrimSpace(strings.ToLower(values[key]))
	return value == "1" || value == "true" || value == "yes"
}

func Summary(values map[string]string, keys ...string) string {
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		if value := strings.TrimSpace(values[key]); value != "" {
			parts = append(parts, fmt.Sprintf("%s=%s", key, value))
		}
	}
	return strings.Join(parts, ", ")
}
