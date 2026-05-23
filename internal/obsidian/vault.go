// Package obsidian — операции с локальным Obsidian-vault:
// чтение/запись .md с YAML-frontmatter, SHA-256 контента и обход дерева
// с пропуском файлов/папок, имена которых начинаются с точки.
package obsidian

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Frontmatter — обязательные поля метаданных в каждом синхронизируемом файле.
// KaitenUID — основной идентификатор документа в Kaiten API; KaitenID — legacy/int.
type Frontmatter struct {
	KaitenID  int       `yaml:"kaiten_id"`
	KaitenUID string    `yaml:"kaiten_uid,omitempty"`
	KaitenURL string    `yaml:"kaiten_url"`
	Updated   time.Time `yaml:"updated"`
	Type      string    `yaml:"kaiten_type,omitempty"`
}

// File — представление документа на диске.
type File struct {
	AbsPath     string
	RelPath     string // относительно корня vault
	Frontmatter Frontmatter
	Body        string // markdown без frontmatter-блока
	Mtime       time.Time
}

// ContentHash возвращает sha256:<hex> по body файла.
func (f *File) ContentHash() string { return HashBody(f.Body) }

// HashBody — sha256:<hex> по строке.
func HashBody(s string) string {
	h := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(h[:])
}

// IsHiddenPath возвращает true, если ЛЮБАЯ часть относительного пути начинается с точки.
// Используется для исключения скрытых файлов/папок (включая .obsidian, .kaiten-sync).
func IsHiddenPath(rel string) bool {
	rel = filepath.ToSlash(rel)
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." {
			continue
		}
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

// WalkUntracked — все .md файлы без валидного kaiten_id во фронтматтере.
// Полезно для --create-remote: пользователь создал файл в Obsidian, хочет вылить в Kaiten.
// Пропускает файлы в kaiten_files/ и скрытые пути.
func WalkUntracked(vault string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(vault, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(vault, path)
		if rel == "." {
			return nil
		}
		if IsHiddenPath(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Пропускаем папку вложений — там не должно быть «голых» документов.
		if d.IsDir() && (rel == "kaiten_files" || strings.HasPrefix(rel, "kaiten_files"+string(filepath.Separator))) {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".md") {
			return nil
		}
		// Проверяем, что файл НЕ имеет валидного frontmatter с kaiten_id.
		if _, ferr := ReadFile(vault, path); ferr == nil {
			return nil // уже трэкируется, не наш клиент
		}
		out = append(out, path)
		return nil
	})
	return out, err
}

// Walk обходит vault и возвращает все .md файлы, у которых ни одна часть
// относительного пути не начинается с точки.
func Walk(vault string) ([]File, error) {
	var out []File
	err := filepath.WalkDir(vault, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(vault, path)
		if rel == "." {
			return nil
		}
		if IsHiddenPath(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".md") {
			return nil
		}
		f, ferr := ReadFile(vault, path)
		if ferr != nil {
			// Файл без валидного frontmatter — игнорируем (это обычная заметка).
			return nil
		}
		out = append(out, *f)
		return nil
	})
	return out, err
}

// ReadFile читает .md, парсит frontmatter и заполняет File.
// Возвращает ошибку, если frontmatter не найден или нет ни kaiten_id, ни kaiten_uid.
func ReadFile(vault, abs string) (*File, error) {
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	fm, body, err := SplitFrontmatter(data)
	if err != nil {
		return nil, err
	}
	if fm.KaitenID == 0 && fm.KaitenUID == "" {
		return nil, fmt.Errorf("нет kaiten_id/kaiten_uid во frontmatter")
	}
	st, _ := os.Stat(abs)
	rel, _ := filepath.Rel(vault, abs)
	return &File{
		AbsPath:     abs,
		RelPath:     filepath.ToSlash(rel),
		Frontmatter: fm,
		Body:        body,
		Mtime:       st.ModTime(),
	}, nil
}

// SplitFrontmatter извлекает YAML-блок между `---` и тело документа.
func SplitFrontmatter(data []byte) (Frontmatter, string, error) {
	var fm Frontmatter
	scanner := bufio.NewScanner(bytes.NewReader(data))
	// Риск R-05: 8 MB было мало для больших документов. 32 MB — разумный потолок.
	scanner.Buffer(make([]byte, 0, 1024*1024), 32*1024*1024)
	if !scanner.Scan() {
		return fm, "", fmt.Errorf("пустой файл")
	}
	if strings.TrimSpace(scanner.Text()) != "---" {
		return fm, "", fmt.Errorf("нет открывающего ---")
	}
	var head bytes.Buffer
	closed := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "---" {
			closed = true
			break
		}
		head.WriteString(line)
		head.WriteByte('\n')
	}
	if !closed {
		return fm, "", fmt.Errorf("нет закрывающего ---")
	}
	if err := yaml.Unmarshal(head.Bytes(), &fm); err != nil {
		return fm, "", fmt.Errorf("парсинг frontmatter: %w", err)
	}
	var body bytes.Buffer
	for scanner.Scan() {
		body.WriteString(scanner.Text())
		body.WriteByte('\n')
	}
	// Стрипаем ведущие пустые строки после закрывающего ---,
	// иначе hash расходится с hash(body), который мы записали в state
	// (Render добавляет пустую строку разделитель). Это баг #21.
	result := strings.TrimLeft(body.String(), "\n")
	return fm, result, nil
}

// WriteAtomic пишет файл атомарно с fsync (защита от R-02):
// 1) tmp-файл, 2) fsync содержимого, 3) rename, 4) fsync каталога.
// dryRun=true — никаких изменений на диске.
func WriteAtomic(absPath string, fm Frontmatter, body string, dryRun bool) error {
	out, err := Render(fm, body)
	if err != nil {
		return err
	}
	if dryRun {
		return nil
	}
	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := absPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(out); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, absPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if df, dErr := os.Open(dir); dErr == nil {
		_ = df.Sync()
		_ = df.Close()
	}
	return nil
}

// Render собирает .md (frontmatter + body).
func Render(fm Frontmatter, body string) ([]byte, error) {
	y, err := yaml.Marshal(&fm)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.Write(y)
	buf.WriteString("---\n")
	if !strings.HasPrefix(body, "\n") {
		buf.WriteByte('\n')
	}
	buf.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// Sanitize очищает фрагмент имени от запрещённых для FS символов.
func Sanitize(name string) string {
	repl := strings.NewReplacer(
		"/", "-", "\\", "-", ":", "-", "*", "-",
		"?", "", "\"", "'", "<", "(", ">", ")", "|", "-",
	)
	out := strings.TrimSpace(repl.Replace(name))
	if out == "" {
		out = "untitled"
	}
	return out
}
