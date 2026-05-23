package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestSaveLoad_KeyringFallback(t *testing.T) {
	// MockInit делает keyring in-memory — в реальной системе он есть.
	// Для теста на fallback используем сам in-memory keyring, но это
	// проверка happy-path keyring.
	keyring.MockInit()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := &Config{
		BaseURL:  "https://x.kaiten.ru",
		VaultDir: "/tmp/vault",
		SpaceID:  42,
		Token:    "secret-token",
	}

	res, err := Save(cfgPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !res.UsedKeyring {
		t.Errorf("ожидалось UsedKeyring=true")
	}

	// Проверяем, что токен в файл НЕ записался.
	data, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(data), "secret-token") {
		t.Errorf("токен попал в YAML: %s", data)
	}

	// Проверяем права 0600.
	info, _ := os.Stat(cfgPath)
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("ожидались права 0600, получены %#o", mode)
	}

	// Load возвращает токен из keyring.
	loaded, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Token != "secret-token" {
		t.Errorf("токен не подгружен из keyring: %q", loaded.Token)
	}
	if loaded.BaseURL != cfg.BaseURL || loaded.SpaceID != cfg.SpaceID {
		t.Errorf("конфиг повреждён: %+v", loaded)
	}
}

func TestLoad_Missing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if !errors.Is(err, os.ErrNotExist) && !os.IsNotExist(err) {
		t.Errorf("ожидалась ErrNotExist, получено %v", err)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		c       *Config
		wantErr bool
	}{
		{&Config{}, true},
		{&Config{BaseURL: "x"}, true},
		{&Config{BaseURL: "x", Token: "t"}, true},
		{&Config{BaseURL: "x", Token: "t", VaultDir: "v"}, false},
	}
	for i, tc := range cases {
		err := tc.c.Validate()
		if (err != nil) != tc.wantErr {
			t.Errorf("case %d: err=%v wantErr=%v", i, err, tc.wantErr)
		}
	}
}

func TestDefaultConfigPath_NonEmpty(t *testing.T) {
	if DefaultConfigPath() == "" {
		t.Error("DefaultConfigPath не должен быть пустым")
	}
}
