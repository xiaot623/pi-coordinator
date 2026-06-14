package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDecodeConfigAppliesEmbeddedDefaults(t *testing.T) {
	cfg, err := decodeConfig([]byte(`telegram:
  bot_token: token
  group_chat_id: 1
  allowed_users: [1]
`))
	if err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if len(cfg.Plugins) != 1 || cfg.Plugins[0] != "@hahahhh/pi-trace@next" {
		t.Fatalf("unexpected default plugins: %#v", cfg.Plugins)
	}
	if cfg.PluginUpdateIntervalMinutes != 1440 {
		t.Fatalf("unexpected default plugin update interval: %d", cfg.PluginUpdateIntervalMinutes)
	}
	if cfg.Runner.Local.SessionDir == "" {
		t.Fatalf("expected default local session dir")
	}
	if cfg.Runner.Docker.Image != "pi-agent:latest" || cfg.Runner.Docker.Network != "bridge" || cfg.Runner.Docker.AgentMountMode != "rw" {
		t.Fatalf("unexpected default docker config: %#v", cfg.Runner.Docker)
	}
	if cfg.Runner.Docker.AgentDir == "" || cfg.Runner.Docker.SkillsDir == "" {
		t.Fatalf("expected default docker mount paths to be populated: %#v", cfg.Runner.Docker)
	}
	if len(cfg.Runner.Docker.ExtraMounts) == 0 || !strings.HasSuffix(cfg.Runner.Docker.ExtraMounts[0].Host, ".config") || cfg.Runner.Docker.ExtraMounts[0].Mode != "ro" {
		t.Fatalf("expected default ~/.config read-only mount, got %#v", cfg.Runner.Docker.ExtraMounts)
	}
	if cfg.OpenTool != "iterm2" || cfg.Diff.Delivery != "send" {
		t.Fatalf("unexpected defaults: open_tool=%q diff.delivery=%q", cfg.OpenTool, cfg.Diff.Delivery)
	}
}

func TestDockerExtraMountsUnmarshalAndExpand(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home dir: %v", err)
	}
	var cfg Config
	data := []byte(`telegram:
  bot_token: token
  group_chat_id: 1
  allowed_users: [1]
runner:
  docker:
    extra_mounts:
      - "~/data"
      - host: "/tmp/cache"
        mode: rw
`)
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	mounts := expandDockerMounts(cfg.Runner.Docker.ExtraMounts)
	if len(mounts) != 2 {
		t.Fatalf("expected 2 mounts, got %d", len(mounts))
	}
	if mounts[0].Host != filepath.Join(home, "data") || mounts[0].Mode != "ro" || !mounts[0].HomeMapped || mounts[0].HomeSubpath != "data" {
		t.Fatalf("unexpected first mount: %#v", mounts[0])
	}
	if mounts[1].Host != "/tmp/cache" || mounts[1].Mode != "rw" || mounts[1].HomeMapped || mounts[1].HomeSubpath != "" {
		t.Fatalf("unexpected second mount: %#v", mounts[1])
	}
}

func TestDockerExtraMountsRejectInvalidMode(t *testing.T) {
	var cfg Config
	data := []byte(`telegram:
  bot_token: token
  group_chat_id: 1
  allowed_users: [1]
runner:
  docker:
    extra_mounts:
      - host: "/tmp/cache"
        mode: write
`)
	err := yaml.Unmarshal(data, &cfg)
	if err == nil || !strings.Contains(err.Error(), "mode must be ro or rw") {
		t.Fatalf("expected invalid mode error, got %v", err)
	}
}
