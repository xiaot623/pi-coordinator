package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var ErrConfigMissing = errors.New("config missing")

type Config struct {
	Telegram struct {
		BotToken     string  `yaml:"bot_token"`
		GroupChatID  int64   `yaml:"group_chat_id"`
		AllowedUsers []int64 `yaml:"allowed_users"`
	} `yaml:"telegram"`
	Runner struct {
		IdleTimeout Duration `yaml:"idle_timeout"`
		SessionDir  string   `yaml:"session_dir"`
		Binary      string   `yaml:"binary"`
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
		if err := os.WriteFile(paths.ConfigPath, []byte(defaultConfig()), 0o600); err != nil {
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
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, paths, err
	}
	cfg.Runner.SessionDir = ExpandPath(cfg.Runner.SessionDir)
	if cfg.Runner.Binary == "" {
		cfg.Runner.Binary = "pi"
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

func defaultConfig() string {
	return `telegram:
  bot_token: ""
  group_chat_id: 0
  allowed_users: []

runner:
  idle_timeout: 5m
  session_dir: "~/.pi/agent/sessions"
  binary: "pi"

global_model: ""
`
}
