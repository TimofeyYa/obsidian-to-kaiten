// Package logging — настройка slog с ротацией через lumberjack.
package logging

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Setup создаёт slog-логгер: stderr (для TUI/silent) + ротируемый файл.
func Setup(vault string, verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}

	logDir := filepath.Join(vault, ".kaiten-sync", "logs")
	_ = os.MkdirAll(logDir, 0o755)

	fileWriter := &lumberjack.Logger{
		Filename:   filepath.Join(logDir, "sync.log"),
		MaxSize:    5, // MB
		MaxBackups: 5,
		MaxAge:     30, // дней
		Compress:   true,
	}

	mw := io.MultiWriter(os.Stderr, fileWriter)
	h := slog.NewTextHandler(mw, &slog.HandlerOptions{Level: level})
	return slog.New(h)
}

// SetupSilent — как Setup, но без stderr (для cron).
func SetupSilent(vault string, verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	logDir := filepath.Join(vault, ".kaiten-sync", "logs")
	_ = os.MkdirAll(logDir, 0o755)
	fw := &lumberjack.Logger{
		Filename:   filepath.Join(logDir, "sync.log"),
		MaxSize:    5,
		MaxBackups: 5,
		MaxAge:     30,
		Compress:   true,
	}
	h := slog.NewTextHandler(fw, &slog.HandlerOptions{Level: level})
	return slog.New(h)
}
