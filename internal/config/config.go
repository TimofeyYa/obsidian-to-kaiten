// Package config отвечает за загрузку/сохранение конфигурации утилиты,
// CLI-флаги и безопасное хранение Bearer-токена Kaiten в OS keyring
// (с fallback в файл с правами 0600).
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zalando/go-keyring"
	"gopkg.in/yaml.v3"
)

const (
	keyringService = "kaiten-obsidian-sync"
	keyringUser    = "bearer-token"
)

// Flags хранит распарсенные CLI-аргументы.
type Flags struct {
	Vault      string
	ConfigPath string
	Silent     bool
	DryRun     bool
	Verbose    bool
}

// Config — то, что сериализуется в YAML.
type Config struct {
	BaseURL  string `yaml:"base_url"`
	VaultDir string `yaml:"vault_dir"`
	SpaceID  int    `yaml:"space_id"`
	// Token хранится в файле только если keyring недоступен.
	Token string `yaml:"token,omitempty"`
}

// SaveResult — что именно произошло при сохранении конфига.
type SaveResult struct {
	UsedKeyring  bool
	TokenInFile  bool
	KeyringError error
}

// ParseFlags разбирает CLI-флаги.
func ParseFlags() Flags {
	var f Flags
	flag.StringVar(&f.Vault, "vault", "", "путь к Obsidian vault (обязателен при первом запуске)")
	flag.StringVar(&f.ConfigPath, "config", DefaultConfigPath(), "путь к конфигу")
	flag.BoolVar(&f.Silent, "silent", false, "без TUI, для cron")
	flag.BoolVar(&f.DryRun, "dry-run", false, "показать что будет изменено, без записи")
	flag.BoolVar(&f.Verbose, "verbose", false, "подробный лог")
	flag.Parse()
	return f
}

// DefaultConfigPath возвращает путь конфига по умолчанию.
func DefaultConfigPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "kaiten-sync", "config.yaml")
	}
	return filepath.Join(os.Getenv("HOME"), ".config", "kaiten-sync", "config.yaml")
}

// Load читает конфиг и подгружает токен из keyring.
// Возвращает (nil, os.ErrNotExist), если конфига нет — это нормально при первом запуске.
// Если файл существует, но имеет права шире 0600 — печатает предупреждение в stderr.
func Load(path string) (*Config, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	// Предупреждение о слишком открытых правах (Unix).
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		fmt.Fprintf(os.Stderr,
			"предупреждение: %s имеет небезопасные права %#o, рекомендую chmod 600\n",
			path, mode)
	}
	data, err := os.ReadFile(path) //nolint:gosec // путь задаётся пользователем — это норма для CLI
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("парсинг %s: %w", path, err)
	}
	if c.Token == "" {
		if tok, kerr := keyring.Get(keyringService, keyringUser); kerr == nil {
			c.Token = tok
		}
	}
	return &c, nil
}

// Save сохраняет конфиг в YAML и пытается записать токен в keyring.
// Если keyring недоступен — токен остаётся в YAML (файл получит права 0600).
// Возвращает SaveResult для прозрачной диагностики вызывающей стороне.
func Save(path string, c *Config) (SaveResult, error) {
	var res SaveResult
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return res, err
	}

	out := *c
	// Пробуем keyring.
	if err := keyring.Set(keyringService, keyringUser, c.Token); err == nil {
		out.Token = "" // в файл токен не пишем
		res.UsedKeyring = true
	} else {
		res.KeyringError = err
		res.TokenInFile = true
	}

	data, err := yaml.Marshal(&out)
	if err != nil {
		return res, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return res, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return res, err
	}
	return res, nil
}

// Validate проверяет обязательные поля.
func (c *Config) Validate() error {
	if c.BaseURL == "" {
		return errors.New("base_url пуст")
	}
	if c.Token == "" {
		return errors.New("token пуст")
	}
	if c.VaultDir == "" {
		return errors.New("vault_dir пуст")
	}
	return nil
}
