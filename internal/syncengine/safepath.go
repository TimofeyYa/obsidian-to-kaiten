package syncengine

import (
	"fmt"
	"path/filepath"
	"strings"
)

// SafeJoin безопасно соединяет vault с относительным путём. Возвращает абсолютный путь.
// Возвращает ошибку, если результирующий путь выходит за пределы vault (path traversal,
// риск R-04). Гарантирует, что результат — потомок vault'а.
func SafeJoin(vault, relPath string) (string, error) {
	vaultAbs, err := filepath.Abs(vault)
	if err != nil {
		return "", err
	}
	// Чистим: убираем `..`, `/./`, и т.п.
	cleaned := filepath.Clean(filepath.Join(vaultAbs, relPath))
	// Проверяем, что результат внутри vault.
	rel, err := filepath.Rel(vaultAbs, cleaned)
	if err != nil {
		return "", fmt.Errorf("вычисление относительного пути: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || strings.Contains(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("путь %q выходит за пределы vault (path traversal)", relPath)
	}
	if filepath.IsAbs(relPath) {
		return "", fmt.Errorf("ожидался относительный путь, получен абсолютный: %q", relPath)
	}
	return cleaned, nil
}
