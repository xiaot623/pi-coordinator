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
