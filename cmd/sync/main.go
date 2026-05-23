// Command kaiten-sync — двусторонняя синхронизация документов Kaiten ↔ Obsidian.
//
// Использование:
//
//	kaiten-sync --vault /path/to/vault              # первый запуск с TUI-визардом
//	kaiten-sync --silent                            # тихий синк для cron
//	kaiten-sync --dry-run --verbose                 # показать что будет изменено
//	kaiten-sync --silent --timeout 10m              # жёсткий таймаут (риск R-16)
//
// Exit codes:
//
//	0 — синк завершён без ошибок (Errors == 0).
//	1 — фатальная ошибка (не удалось загрузить конфиг, обойти vault, и т.п.).
//	2 — синк прошёл, но в отчёте есть Errors > 0 (cron может алертить).
//	3 — другой экземпляр kaiten-sync уже работает с этим vault'ом.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/timofeyblog/kaiten-obsidian-sync/internal/config"
	"github.com/timofeyblog/kaiten-obsidian-sync/internal/kaiten"
	"github.com/timofeyblog/kaiten-obsidian-sync/internal/logging"
	"github.com/timofeyblog/kaiten-obsidian-sync/internal/syncengine"
	"github.com/timofeyblog/kaiten-obsidian-sync/internal/tui"
)

// Exit codes.
const (
	exitOK            = 0
	exitFatal         = 1
	exitSyncedWithErr = 2
	exitAlreadyRun    = 3
)

// extraFlags содержит флаги сверх базовых config.Flags.
type extraFlags struct {
	Timeout            time.Duration
	CreateRemote       bool
	DeleteOrphans      bool
	DeleteLocalOrphans bool
}

func main() {
	var extra extraFlags
	flag.DurationVar(&extra.Timeout, "timeout", 10*time.Minute,
		"жёсткий таймаут выполнения одного прохода синка (защита от подвисших cron-запусков)")
	flag.BoolVar(&extra.CreateRemote, "create-remote", false,
		"создавать в Kaiten новые документы, которые вы создали локально в Obsidian")
	flag.BoolVar(&extra.DeleteOrphans, "delete-orphans", false,
		"удалять в Kaiten документы, исчезнувшие из vault (ОПАСНО: проверьте --dry-run сначала)")
	flag.BoolVar(&extra.DeleteLocalOrphans, "delete-local-orphans", false,
		"удалять локальные .md для документов, удалённых в Kaiten")
	flags := config.ParseFlags() // вызывает flag.Parse внутри
	code := run(flags, extra)
	os.Exit(code)
}

// run возвращает exit code.
//
//nolint:funlen // это оркестратор — разбивка ухудшит читаемость
func run(flags config.Flags, extra extraFlags) int {
	// Сигналы (SIGINT/SIGTERM) + жёсткий timeout (R-16).
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if extra.Timeout > 0 {
		var tcancel context.CancelFunc
		ctx, tcancel = context.WithTimeout(ctx, extra.Timeout)
		defer tcancel()
	}

	cfg, err := config.Load(flags.ConfigPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) && !os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		return exitFatal
	}

	// Silent-режим: конфиг обязателен.
	if flags.Silent {
		if cfg == nil {
			fmt.Fprintln(os.Stderr, "--silent требует существующего конфига (запустите без --silent для настройки)")
			return exitFatal
		}
		if err := cfg.Validate(); err != nil {
			fmt.Fprintln(os.Stderr, "невалидный конфиг:", err)
			return exitFatal
		}
		return doSync(ctx, cfg, flags, extra, logging.SetupSilent(cfg.VaultDir, flags.Verbose))
	}

	// Интерактивный режим: если конфига нет либо vault меняется — гоняем TUI.
	needWizard := cfg == nil || cfg.Token == "" || cfg.BaseURL == ""
	if flags.Vault != "" && cfg != nil && cfg.VaultDir != flags.Vault {
		cfg.VaultDir = flags.Vault
	}

	if needWizard {
		base := ""
		if cfg != nil {
			base = cfg.BaseURL
		}
		res, terr := tui.Run(base)
		if terr != nil {
			fmt.Fprintln(os.Stderr, "ошибка TUI:", terr)
			return exitFatal
		}
		if res.Aborted {
			fmt.Println("отменено")
			return exitOK
		}
		vault := flags.Vault
		if vault == "" && cfg != nil {
			vault = cfg.VaultDir
		}
		if vault == "" {
			fmt.Fprintln(os.Stderr, "--vault обязателен при первом запуске")
			return exitFatal
		}
		// Warning о подозрительном домене (риск R-11).
		if !syncengine.IsLikelyKaitenURL(res.BaseURL) {
			fmt.Fprintf(os.Stderr,
				"внимание: %q не похож на Kaiten-инстанс (.kaiten.ru / .kaiten.app). "+
					"Если это on-prem — игнорируйте.\n", res.BaseURL)
		}
		cfg = &config.Config{
			BaseURL:  res.BaseURL,
			Token:    res.Token,
			RootUID:  res.RootUID,
			VaultDir: vault,
		}
		saveRes, serr := config.Save(flags.ConfigPath, cfg)
		if serr != nil {
			fmt.Fprintln(os.Stderr, "сохранение конфига:", serr)
			return exitFatal
		}
		fmt.Printf("конфиг сохранён в %s\n", flags.ConfigPath)
		if !saveRes.UsedKeyring {
			fmt.Fprintf(os.Stderr,
				"внимание: OS keyring недоступен (%v), токен сохранён в %s с правами 0600\n",
				saveRes.KeyringError, flags.ConfigPath)
		}
	}

	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "невалидный конфиг:", err)
		return exitFatal
	}

	logger := logging.Setup(cfg.VaultDir, flags.Verbose)
	return doSync(ctx, cfg, flags, extra, logger)
}

func doSync(ctx context.Context, cfg *config.Config, flags config.Flags, extra extraFlags, logger *slog.Logger) int {
	// Preflight: проверяем доступность vault на запись (риск R-10).
	if err := syncengine.Preflight(cfg.VaultDir); err != nil {
		logger.Error("preflight failed", "err", err)
		fmt.Fprintln(os.Stderr, "preflight:", err)
		return exitFatal
	}

	// Эксклюзивный лок на vault (риск R-01).
	// В DryRun лок тоже берём — иначе можно случайно мешать настоящему синку.
	lock, err := syncengine.AcquireVaultLock(cfg.VaultDir)
	if err != nil {
		if errors.Is(err, syncengine.ErrAlreadyRunning) {
			logger.Warn("другой экземпляр уже работает — выхожу", "vault", cfg.VaultDir)
			fmt.Fprintln(os.Stderr, "другой экземпляр kaiten-sync уже работает с этим vault'ом")
			return exitAlreadyRun
		}
		logger.Error("не удалось взять lock", "err", err)
		fmt.Fprintln(os.Stderr, "lock:", err)
		return exitFatal
	}
	defer func() { _ = lock.Release() }()

	state, err := syncengine.LoadState(cfg.VaultDir)
	if err != nil {
		logger.Error("загрузка state", "err", err)
		fmt.Fprintln(os.Stderr, "загрузка state:", err)
		return exitFatal
	}

	client := kaiten.New(cfg.BaseURL, cfg.Token)
	eng := &syncengine.Engine{
		Vault:              cfg.VaultDir,
		BaseURL:            cfg.BaseURL,
		RootUID:            cfg.RootUID,
		Client:             client,
		State:              state,
		Logger:             logger,
		DryRun:             flags.DryRun,
		CreateRemote:       extra.CreateRemote,
		DeleteOrphans:      extra.DeleteOrphans,
		DeleteLocalOrphans: extra.DeleteLocalOrphans,
	}
	logger.Info("старт синка", "vault", cfg.VaultDir, "root_uid", cfg.RootUID, "dry_run", flags.DryRun)
	rep, err := eng.Run(ctx, cfg.RootUID)
	if err != nil {
		logger.Error("синк завершился с ошибкой", "err", err)
		fmt.Fprintln(os.Stderr, "синк завершился с ошибкой:", err)
		// Даже если Run упал, мы попытаемся сохранить state с LastError —
		// если он есть в памяти и не DryRun.
		if !flags.DryRun {
			_ = syncengine.SaveState(cfg.VaultDir, state)
		}
		return exitFatal
	}
	logger.Info("синк завершён", "report", rep.String())
	if !flags.Silent {
		fmt.Println(rep.String())
	}
	if rep.HasErrors() {
		return exitSyncedWithErr
	}
	return exitOK
}
