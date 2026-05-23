# kaiten-obsidian-sync

Кросс-платформенная консольная утилита на Go, которая выполняет двустороннюю синхронизацию документов из [Kaiten](https://kaiten.ru) с локальным хранилищем [Obsidian](https://obsidian.md) на основе Markdown-файлов.

- Только сущности `document` уровня пространства (карточки, доски, чек-листы, комментарии игнорируются).
- Точечное обновление: переписываются только изменённые документы.
- Файлы и папки на `.` (точку) не трогаются — `.obsidian/`, `.kaiten-sync/` остаются нетронутыми.
- Интерактивный визард на Bubble Tea при первом запуске; тихий режим для cron.

## Требования

- Go 1.22+
- macOS или Linux (Windows: см. раздел «Cron»)
- Доступ к инстансу Kaiten и Bearer-токен из настроек профиля

## Установка

```bash
git clone https://github.com/timofeyblog/kaiten-obsidian-sync.git
cd kaiten-obsidian-sync
make build
sudo make install      # копирует bin/kaiten-sync в /usr/local/bin
```

## Первый запуск

```bash
kaiten-sync --vault /path/to/obsidian-vault
```

Откроется TUI-визард:

1. Ввод базового URL Kaiten (`https://mycompany.kaiten.ru`).
2. Ввод Bearer-токена (из настроек профиля Kaiten).
3. Проверка соединения через `GET /api/latest/users/current`.
4. Выбор корневого пространства из списка `GET /api/latest/spaces`.
5. Запуск первичной синхронизации.

После успешного визарда конфиг сохраняется в `~/.config/kaiten-sync/config.yaml` (права `0600`), а токен — в OS keyring (Keychain / Secret Service / Windows Credential Manager). Если keyring недоступен — токен остаётся в YAML-файле.

## Регулярный запуск (cron, каждые 15 минут)

```bash
make cron-install
```

Эта команда добавит в crontab:

```cron
*/15 * * * * /usr/local/bin/kaiten-sync --silent --config ~/.config/kaiten-sync/config.yaml >> ~/.kaiten-sync/cron.log 2>&1
```

В `--silent` режиме TUI не открывается, конфиг читается из файла. Удалить задачу: `make cron-uninstall`.

**Windows**: используйте Task Scheduler. Создайте задачу с триггером «каждые 15 минут» и действием `kaiten-sync.exe --silent --config %APPDATA%\kaiten-sync\config.yaml`.

## CLI-флаги

| Флаг | По умолчанию | Описание |
|------|--------------|----------|
| `--vault <path>` | — | Путь к Obsidian vault (обязателен при первом запуске) |
| `--config <path>` | `~/.config/kaiten-sync/config.yaml` | Путь к конфигу |
| `--silent` | `false` | Без TUI, для cron |
| `--dry-run` | `false` | Показать что будет изменено, без записи |
| `--verbose` | `false` | Подробный лог (slog DEBUG) |
| `--timeout <dur>` | `10m` | Жёсткий таймаут одного прохода (защита от зависших cron-запусков) |

## Exit codes

- `0` — синк успешен.
- `1` — фатальная ошибка (конфиг/сеть/FS).
- `2` — синк прошёл, но в отчёте есть ошибки (cron может алертить).
- `3` — другой экземпляр уже работает с этим vault'ом (защита от гонки cron-запусков).

## Безопасность и надёжность

- **Токен** хранится в OS keyring (при недоступности — в файле с правами `0600`), маскируется в логах.
- **Локи**: `flock` на `<vault>/.kaiten-sync/lock` — два параллельных cron-запуска не пересекутся.
- **Атомарные записи** state и .md: tmp → `fsync` → rename → `fsync` каталога. Сбой питания не повредит файл.
- **Path traversal** блокируется: любые `..`/абсолютные пути в `Document.Path` отвергаются.
- **Конфликты**: бэкап пишется в `.<base>.conflict-<ts>.md` (ведущая точка → не синхронизируется). Если бэкап не создан — pull НЕ выполняется.
- **Healthcheck**: в `state.json` поля `last_success` и `last_error` — можно видеть, давно ли синк работал.
- **Лимит ответа** Kaiten: 64 MB (`io.LimitReader`) — OOM невозможен.
- **Preflight** проверяет доступ к vault на запись до захвата лока.

Полный риск-реестр и митигации — в [`RISKS.md`](RISKS.md).

## Известные ограничения

- **Гонка с Obsidian (R-09).** Если вы редактируете файл в Obsidian в момент синка, без сохранения — последующее сохранение Obsidian может затереть обновление от Kaiten. При следующем синке ситуация будет разобрана как `Conflict` — ваши изменения сохранятся в `.<имя>.conflict-*.md`.
- **Осиротевшие файлы (R-08).** Если документ удалён в Kaiten, локальный .md остаётся и в лог пишется WARN. Двусторонняя синхронизация для этого файла больше не выполняется — это просто копия.

## Формат файлов в Obsidian

Каждый Kaiten-документ → один `.md` с YAML-frontmatter:

```markdown
---
kaiten_id: 123
kaiten_url: https://mycompany.kaiten.ru/document/123
updated: 2026-05-23T10:00:00Z
kaiten_type: html
---

# Заголовок документа

Содержимое в Markdown…
```

Иерархия пространств/папок Kaiten отражается через структуру каталогов vault. Документы типа `html` конвертируются в Markdown при загрузке через [html-to-markdown](https://github.com/JohannesKaufmann/html-to-markdown) и обратно — при выгрузке.

## Алгоритм синхронизации

1. Загружается `state.json` и конфиг.
2. Через `GET /spaces/{id}/documents` собирается дерево документов уровня пространства.
3. Vault обходится рекурсивно (`.md` файлы, пути на `.` пропускаются).
4. Файлы матчатся с документами по `kaiten_id` из frontmatter.
5. Для каждого документа определяется направление:
   - `unchanged` — ничего не делать;
   - `remote_newer` — Kaiten → Obsidian (перезаписать `.md`);
   - `local_newer` — Obsidian → Kaiten (`PATCH /documents/{id}`);
   - `conflict` — локальная сохраняется как `<имя>.conflict-<timestamp>.md`, Kaiten берётся как основная, событие логируется;
   - `new_remote` — создать `.md` локально;
   - `deleted_remote` — был в state, исчез в Kaiten → запись из state удаляется, локальный файл остаётся.
6. Запросы к Kaiten — с rate-limit ≤5 req/sec и ретраями на 5xx / 429.
7. `state.json` сохраняется атомарно (tmp + rename).
8. В лог пишется отчёт: `synced=X uploaded=Y conflicts=Z`.

## Структура state.json

`vault/.kaiten-sync/state.json` — не попадает под синк, так как имя начинается с точки.

```json
{
  "documents": {
    "123": {
      "path": "Notes/Project A.md",
      "kaiten_updated": "2026-05-23T10:00:00Z",
      "local_mtime": "2026-05-23T10:00:00Z",
      "content_hash": "sha256:..."
    }
  },
  "last_sync": "2026-05-23T15:26:00+07:00",
  "space_id": 12345
}
```

## Структура проекта

```
kaiten-obsidian-sync/
├── cmd/sync/main.go         # точка входа
├── internal/
│   ├── config/              # CLI-флаги, YAML-конфиг, keyring
│   ├── kaiten/              # REST-клиент: spaces, documents, retry, rate-limit
│   ├── obsidian/            # vault: frontmatter, hash, walk, atomic write
│   ├── syncengine/          # diff, conflict, state, HTML↔MD
│   ├── tui/                 # Bubble Tea: login → space picker → progress
│   └── logging/             # slog + lumberjack ротация
├── Makefile
├── go.mod
└── README.md
```

## Тесты и линт

```bash
make test     # go test ./... -race -cover
make lint     # golangci-lint run (требуется установка)
make fmt      # gofmt + goimports
```

## Замечания по Kaiten API

Точные пути endpoints для документов (`/spaces/{id}/documents`, `/documents/{id}`) использованы по REST-конвенции Kaiten — публичная страница `https://developers.kaiten.ru/` показывает только базовые параметры (Bearer-auth, `/api/latest`, rate-limit 5 req/sec). Если в конкретном инстансе пути отличаются — скорректируйте константы `DocsListPath` и `DocPath` в `internal/kaiten/client.go`.

## Ограничения MVP

- Создание новых документов из Obsidian в Kaiten (`NewLocal`) не реализовано — только логируется.
- Конвертация Markdown → HTML для обратной выгрузки упрощённая (через `<br/>`). Для полноценного рендера подключите `goldmark` в `internal/syncengine/convert.go::MarkdownToHTML`.
- Документы внутри карточек Kaiten игнорируются (см. `Document.IsSpaceLevel()`).

## Лицензия

MIT
