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

var defaultRunnerPlugins = []string{"@hahahhh/pi-trace@next"}

const defaultPluginUpdateIntervalMinutes = 1440

type Config struct {
	Telegram struct {
		BotToken     string  `yaml:"bot_token"`
		GroupChatID  int64   `yaml:"group_chat_id"`
		AllowedUsers []int64 `yaml:"allowed_users"`
	} `yaml:"telegram"`
	Runner struct {
		IdleTimeout                 Duration `yaml:"idle_timeout"`
		SessionDir                  string   `yaml:"session_dir"`
		Binary                      string   `yaml:"binary"`
		DockerImage                 string   `yaml:"docker_image"`
		Plugins                     []string `yaml:"plugins"`
		PluginUpdateIntervalMinutes int      `yaml:"plugin_update_interval_minutes"`
	} `yaml:"runner"`
	GlobalModel string `yaml:"global_model"`
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
	cfg := Config{}
	cfg.Runner.IdleTimeout.Duration = 5 * time.Minute
	cfg.Runner.SessionDir = "~/.pi/agent/sessions"
	cfg.Runner.Binary = "pi"
	cfg.Runner.DockerImage = "pi-agent:latest"
	cfg.Runner.Plugins = append([]string(nil), defaultRunnerPlugins...)
	cfg.Runner.PluginUpdateIntervalMinutes = defaultPluginUpdateIntervalMinutes
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, paths, err
	}
	cfg.Runner.SessionDir = ExpandPath(cfg.Runner.SessionDir)
	cfg.Runner.Plugins = expandPaths(cfg.Runner.Plugins)
	if cfg.Runner.Binary == "" {
		cfg.Runner.Binary = "pi"
	}
	if cfg.Runner.DockerImage == "" {
		cfg.Runner.DockerImage = "pi-agent:latest"
	}
	if cfg.Telegram.BotToken == "" || cfg.Telegram.GroupChatID == 0 || len(cfg.Telegram.AllowedUsers) == 0 {
		return Config{}, paths, fmt.Errorf("telegram.bot_token, telegram.group_chat_id, and telegram.allowed_users are required in %s", paths.ConfigPath)
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
					// We read directly instead of Load() because Load() re-checks defaults
					// But Load() also expands paths. We should probably just call a private load or re-resolve.
					// Actually, the easiest is to read the file and Unmarshal again.
					data, err := os.ReadFile(configPath)
					if err != nil {
						onChange(Config{}, err)
						continue
					}
					var cfg Config
					cfg.Runner.IdleTimeout.Duration = 5 * time.Minute
					cfg.Runner.SessionDir = "~/.pi/agent/sessions"
					cfg.Runner.Binary = "pi"
					cfg.Runner.DockerImage = "pi-agent:latest"
					cfg.Runner.Plugins = append([]string(nil), defaultRunnerPlugins...)
					cfg.Runner.PluginUpdateIntervalMinutes = defaultPluginUpdateIntervalMinutes
					if err := yaml.Unmarshal(data, &cfg); err != nil {
						onChange(Config{}, err)
						continue
					}
					cfg.Runner.SessionDir = ExpandPath(cfg.Runner.SessionDir)
					cfg.Runner.Plugins = expandPaths(cfg.Runner.Plugins)
					if cfg.Runner.Binary == "" {
						cfg.Runner.Binary = "pi"
					}
					if cfg.Runner.DockerImage == "" {
						cfg.Runner.DockerImage = "pi-agent:latest"
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

//go:embed default_config.yaml
var defaultConfig []byte
