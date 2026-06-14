package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerContainerArgsAllowSessionWithoutWorkspaceMount(t *testing.T) {
	tmp := t.TempDir()
	agentDir := filepath.Join(tmp, "agent")
	pluginDir := filepath.Join(tmp, "plugins")
	sessionDir := filepath.Join(tmp, "sessions")
	for _, dir := range []string{agentDir, pluginDir, sessionDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("prepare dir %s: %v", dir, err)
		}
	}

	d := NewDocker(DockerOptions{
		Binary:         "pi",
		Image:          "pi-agent:test",
		HostAgentDir:   agentDir,
		HostPluginDir:  pluginDir,
		HostSessionDir: sessionDir,
	})

	args, err := d.containerArgs(context.Background(), StartRequest{SessionID: "sess-1"})
	if err != nil {
		t.Fatalf("containerArgs returned error: %v", err)
	}

	joined := strings.Join(args, " ")
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-v" {
			continue
		}
		if strings.HasPrefix(args[i+1], ":") {
			t.Fatalf("unexpected empty workspace mount in args: %q", joined)
		}
	}
	if !strings.Contains(joined, "-w /workspace") {
		t.Fatalf("expected default workdir in args: %q", joined)
	}
}

func TestDockerContainerArgsIncludeExtraMounts(t *testing.T) {
	tmp := t.TempDir()
	agentDir := filepath.Join(tmp, "agent")
	pluginDir := filepath.Join(tmp, "plugins")
	sessionDir := filepath.Join(tmp, "sessions")
	roDir := filepath.Join(tmp, "shared")
	rwDir := filepath.Join(tmp, "scratch")
	for _, dir := range []string{agentDir, pluginDir, sessionDir, roDir, rwDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("prepare dir %s: %v", dir, err)
		}
	}

	d := NewDocker(DockerOptions{
		Binary:         "pi",
		Image:          "pi-agent:test",
		HostAgentDir:   agentDir,
		HostPluginDir:  pluginDir,
		HostSessionDir: sessionDir,
		ExtraMounts: []DockerMount{
			{HostPath: roDir},
			{HostPath: rwDir, Mode: "rw"},
		},
	})

	args, err := d.containerArgs(context.Background(), StartRequest{SessionID: "sess-1"})
	if err != nil {
		t.Fatalf("containerArgs returned error: %v", err)
	}

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, roDir+":"+roDir+":ro") {
		t.Fatalf("expected read-only extra mount in args: %q", joined)
	}
	if !strings.Contains(joined, rwDir+":"+rwDir+":rw") {
		t.Fatalf("expected read-write extra mount in args: %q", joined)
	}
}

func TestDockerContainerArgsRejectOverlappingExtraMounts(t *testing.T) {
	tmp := t.TempDir()
	agentDir := filepath.Join(tmp, "agent")
	pluginDir := filepath.Join(tmp, "plugins")
	sessionDir := filepath.Join(tmp, "sessions")
	for _, dir := range []string{agentDir, pluginDir, sessionDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("prepare dir %s: %v", dir, err)
		}
	}

	d := NewDocker(DockerOptions{
		Binary:         "pi",
		Image:          "pi-agent:test",
		HostAgentDir:   agentDir,
		HostPluginDir:  pluginDir,
		HostSessionDir: sessionDir,
		ExtraMounts:    []DockerMount{{HostPath: agentDir}},
	})

	_, err := d.containerArgs(context.Background(), StartRequest{SessionID: "sess-1"})
	if err == nil || !strings.Contains(err.Error(), "overlaps reserved mount") {
		t.Fatalf("expected overlap error, got %v", err)
	}
}
