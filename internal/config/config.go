package config

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

var ErrConfigMissing = errors.New("config missing")

type Config struct {
	Telegram struct {
		BotToken     string  `yaml:"bot_token"`
		GroupChatID  int64   `yaml:"group_chat_id"`
		AllowedUsers []int64 `yaml:"allowed_users"`
	} `yaml:"telegram"`
	Plugins                     []string `yaml:"plugins"`
	PluginUpdateIntervalMinutes int      `yaml:"plugin_update_interval_minutes"`
	Runner                      struct {
		Local    RunnerConfig `yaml:"local"`
		Worktree RunnerConfig `yaml:"worktree"`
		Docker   DockerConfig `yaml:"docker"`
	} `yaml:"runner"`
	OpenTool    string `yaml:"open_tool"`
	GlobalModel string `yaml:"global_model"`
	Diff        struct {
		Delivery string `yaml:"delivery"` // send | open | all
	} `yaml:"diff"`
}

type RunnerConfig struct {
	IdleTimeout Duration `yaml:"idle_timeout"`
	SessionDir  string   `yaml:"session_dir"`
}

type DockerConfig struct {
	IdleTimeout    Duration     `yaml:"idle_timeout"`
	Image          string       `yaml:"image"`
	Network        string       `yaml:"network"`
	AgentDir       string       `yaml:"agent_dir"`
	SkillsDir      string       `yaml:"skills_dir"`
	AgentMountMode string       `yaml:"agent_mount_mode"`
	ExtraMounts    DockerMounts `yaml:"extra_mounts"`
}

// DockerMount describes one host directory bind-mounted into the container at
// the same absolute path.
type DockerMount struct {
	Host string `yaml:"host"`
	Mode string `yaml:"mode"`
}

type DockerMounts []DockerMount

func (m *DockerMounts) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.SequenceNode {
		return fmt.Errorf("docker.extra_mounts must be a sequence")
	}
	mounts := make([]DockerMount, 0, len(value.Content))
	for i, item := range value.Content {
		var mount DockerMount
		switch item.Kind {
		case yaml.ScalarNode:
			if err := item.Decode(&mount.Host); err != nil {
				return err
			}
			mount.Mode = "ro"
		case yaml.MappingNode:
			if err := item.Decode(&mount); err != nil {
				return err
			}
			if strings.TrimSpace(mount.Mode) == "" {
				mount.Mode = "ro"
			}
		default:
			return fmt.Errorf("docker.extra_mounts[%d] must be a string or mapping", i)
		}
		mount.Host = strings.TrimSpace(mount.Host)
		mount.Mode = strings.TrimSpace(mount.Mode)
		if mount.Host == "" {
			return fmt.Errorf("docker.extra_mounts[%d].host is required", i)
		}
		if mount.Mode != "ro" && mount.Mode != "rw" {
			return fmt.Errorf("docker.extra_mounts[%d].mode must be ro or rw", i)
		}
		mounts = append(mounts, mount)
	}
	*m = mounts
	return nil
}

type Paths struct {
	DataDir    string
	DBPath     string
	ConfigPath string
	Dev        bool
}

type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var raw string
	if err := value.Decode(&raw); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return err
	}
	d.Duration = parsed
	return nil
}

func Load() (Config, Paths, error) {
	paths, err := ResolvePaths()
	if err != nil {
		return Config{}, Paths{}, err
	}
	if _, err := os.Stat(paths.ConfigPath); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(paths.ConfigPath), 0o755); err != nil {
			return Config{}, paths, err
		}
		if err := os.WriteFile(paths.ConfigPath, defaultConfig, 0o600); err != nil {
			return Config{}, paths, err
		}
		return Config{}, paths, ErrConfigMissing
	}
	data, err := os.ReadFile(paths.ConfigPath)
	if err != nil {
		return Config{}, paths, err
	}
	cfg, err := decodeConfig(data)
	if err != nil {
		return Config{}, paths, err
	}
	if err := validateConfig(cfg, paths.ConfigPath); err != nil {
		return Config{}, paths, err
	}
	return cfg, paths, nil
}

func SetGlobalModel(path, model string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	if len(doc.Content) == 0 {
		doc.Kind = yaml.DocumentNode
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("config root must be a YAML mapping")
	}
	value := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: model}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "global_model" {
			root.Content[i+1] = value
			return writeYAML(path, &doc)
		}
	}
	root.Content = append(root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "global_model"},
		value,
	)
	return writeYAML(path, &doc)
}

func writeYAML(path string, doc *yaml.Node) error {
	data, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func Watch(ctx context.Context, configPath string, onChange func(Config, error)) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	if err := watcher.Add(configPath); err != nil {
		watcher.Close()
		return err
	}

	go func() {
		defer watcher.Close()
		var lastReload time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					// Debounce quick writes (e.g., from some editors like vim)
					if time.Since(lastReload) < 500*time.Millisecond {
						continue
					}
					lastReload = time.Now()

					// Re-load the config
					data, err := os.ReadFile(configPath)
					if err != nil {
						onChange(Config{}, err)
						continue
					}
					cfg, err := decodeConfig(data)
					if err != nil {
						onChange(Config{}, err)
						continue
					}
					if err := validateConfig(cfg, configPath); err != nil {
						onChange(Config{}, err)
						continue
					}
					onChange(cfg, nil)
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				onChange(Config{}, err)
			}
		}
	}()
	return nil
}

func decodeConfig(data []byte) (Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(defaultConfig, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse embedded default config: %w", err)
	}
	if len(data) > 0 {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return Config{}, err
		}
	}
	normalizeConfig(&cfg)
	return cfg, nil
}

func normalizeConfig(cfg *Config) {
	cfg.Runner.Local.SessionDir = ExpandPath(cfg.Runner.Local.SessionDir)
	cfg.Runner.Worktree.SessionDir = ExpandPath(cfg.Runner.Worktree.SessionDir)
	cfg.Runner.Docker.AgentDir = ExpandPath(cfg.Runner.Docker.AgentDir)
	cfg.Runner.Docker.SkillsDir = ExpandPath(cfg.Runner.Docker.SkillsDir)
	cfg.Runner.Docker.ExtraMounts = expandDockerMounts(cfg.Runner.Docker.ExtraMounts)
	cfg.Plugins = expandPaths(cfg.Plugins)
}

func validateConfig(cfg Config, path string) error {
	if cfg.Telegram.BotToken == "" || cfg.Telegram.GroupChatID == 0 || len(cfg.Telegram.AllowedUsers) == 0 {
		return fmt.Errorf("telegram.bot_token, telegram.group_chat_id, and telegram.allowed_users are required in %s", path)
	}
	return nil
}

func ResolvePaths() (Paths, error) {
	if os.Getenv("PICO_ENV") == "dev" {
		wd, err := os.Getwd()
		if err != nil {
			return Paths{}, err
		}
		dir := filepath.Join(wd, "dev_assets")
		return Paths{DataDir: dir, DBPath: filepath.Join(dir, "pico.db"), ConfigPath: filepath.Join(dir, "pico.config.yaml"), Dev: true}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	dir := filepath.Join(home, ".pi", "pico")
	return Paths{DataDir: dir, DBPath: filepath.Join(dir, "pico.db"), ConfigPath: filepath.Join(dir, "config.yaml")}, nil
}

func ExpandPath(path string) string {
	if path == "~" {
		home, _ := os.UserHomeDir()
		return home
	}
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}

func expandPaths(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	expanded := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		expanded = append(expanded, ExpandPath(path))
	}
	return expanded
}

func expandDockerMounts(mounts DockerMounts) DockerMounts {
	if len(mounts) == 0 {
		return nil
	}
	expanded := make(DockerMounts, 0, len(mounts))
	for _, mount := range mounts {
		host := strings.TrimSpace(mount.Host)
		if host == "" {
			continue
		}
		expanded = append(expanded, DockerMount{
			Host: ExpandPath(host),
			Mode: strings.TrimSpace(mount.Mode),
		})
	}
	return expanded
}

//go:embed default_config.yaml
var defaultConfig []byte
