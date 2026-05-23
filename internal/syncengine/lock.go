package syncengine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

// ErrAlreadyRunning возвращается, если другой экземпляр держит лок на vault.
var ErrAlreadyRunning = errors.New("другой процесс kaiten-sync уже работает с этим vault'ом")

// VaultLock — обёртка над файловым локом на <vault>/.kaiten-sync/lock.
// Защита от R-01 (гонка cron-запусков).
type VaultLock struct {
	fl *flock.Flock
}

// AcquireVaultLock пытается захватить эксклюзивный лок (non-blocking).
// Возвращает ErrAlreadyRunning, если другой процесс уже держит лок.
func AcquireVaultLock(vault string) (*VaultLock, error) {
	dir := filepath.Join(vault, ".kaiten-sync")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("создание .kaiten-sync: %w", err)
	}
	lockPath := filepath.Join(dir, "lock")
	fl := flock.New(lockPath)
	ok, err := fl.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	if !ok {
		return nil, ErrAlreadyRunning
	}
	return &VaultLock{fl: fl}, nil
}

// Release снимает лок. Безопасно вызывать defer'ом.
func (l *VaultLock) Release() error {
	if l == nil || l.fl == nil {
		return nil
	}
	return l.fl.Unlock()
}
