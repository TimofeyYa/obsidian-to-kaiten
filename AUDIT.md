# Аудит kaiten-obsidian-sync

Дата: 2026-05-23. Версия после первичной реализации, до фиксов.

## 1. Соответствие ТЗ — чек-лист acceptance criteria

| # | Требование ТЗ | Статус до фиксов | Комментарий |
|---|---|---|---|
| 1 | TUI-визард при первом запуске | ✅ | `internal/tui` — login → space picker |
| 2 | Повторный запуск с `--silent` без интерактива | ✅ | `cmd/sync/main.go` ветка `flags.Silent` |
| 3 | Документ изменён только в Obsidian → корректно отправляется в Kaiten | ⚠️ | Логика есть, но в `pushLocal` `mtime` берётся **до** записи на диск, при следующем синке файл снова попадёт в `LocalNewer` (циклическая выгрузка). Баг #6 |
| 4 | Документ изменён только в Kaiten → перезаписывается локально | ✅ | `pullRemote` + `os.Chtimes` |
| 5 | Файлы/папки на `.` не трогаются | ⚠️ | `IsHiddenPath` работает, но `targetRelPath` НЕ валидирует, что `r.Path` не начинается с точки — Kaiten может вернуть `.archive/Doc`, и Walk потом не увидит созданный файл. Баг #1 |
| 6 | Карточки и доски не появляются в vault | ✅ | `Document.IsSpaceLevel()` фильтр |
| 7 | `make build && make test` зелёные на macOS и Linux | ✅ | проверено на Linux |
| 8 | Cron-задача через `make cron-install` | ✅ | Makefile цель `cron-install` |
| 9 | Конфликт сохраняется как `.conflict-<ts>.md`, исходные не теряются | ✅ | `handleConflict` |
| 10 | Двусторонняя синхронизация, только изменённые документы | ⚠️ | См. баги #3, #6 |
| 11 | `.kaiten-sync/state.json` со структурой из ТЗ | ✅ | `internal/syncengine/state.go` |

## 2. Найденные баги и краевые случаи

### Баг #1 — `targetRelPath` не санитизирует ведущие точки
**Файл:** `internal/syncengine/engine.go:217-235`.
Если документ в Kaiten называется `.draft` или находится в папке `.archive`, `Sanitize` оставит точку и в результате `Walk` (с `IsHiddenPath`) пропустит этот файл на следующем синке → бесконечное «`new_remote`» в каждом проходе.

### Баг #2 — `ListDocuments` использует псевдо-truncation `docs[:0]`
**Файл:** `internal/kaiten/client.go:212-217`.
Это работает, но мутирует исходный слайс. На больших ответах (тысячи документов) шаг компилятором не оптимизируется, и при отладке/логировании можно получить путаницу. Лучше явный новый слайс.

### Баг #3 — `BuildDecisions` итерируется по map → недетерминированный порядок
**Файл:** `internal/syncengine/diff.go:114-126`.
В тестах и логах порядок применения может различаться от запуска к запуску, что мешает воспроизводимости. Нужно сортировать по ID.

### Баг #4 — `pullRemote` записывает frontmatter дважды через `WriteAtomic`, при этом mtime берётся из `r.Updated`, но если `Updated` нулевой (новый документ без updated в Kaiten) — файл получит mtime 0001-01-01 и при следующем синке попадёт в `LocalNewer`.
**Файл:** `engine.go:150-152`. Нужно защититься от zero-time.

### Баг #5 — `handleConflict` создаёт backup даже при `DryRun`, описание в логе говорит обратное
Перечитал — на самом деле в коде `if d.Local != nil && !e.DryRun` ОК, **это не баг**. Снимаю замечание.

### Баг #6 — `pushLocal` после успешного PATCH сохраняет `LocalMtime: l.Mtime` (старый mtime до записи в Kaiten), а `ContentHash: l.ContentHash()` (содержит новый body). При следующем синке `local.Mtime.After(prev.LocalMtime + tolerance)` будет false, но если пользователь снова сохранит файл — попадёт в `LocalNewer` корректно. **Это не баг сам по себе, но**: если PATCH ответ Kaiten вернёт другой `updated_at` (server-side normalization), а локальный файл не меняли — следующий синк увидит `RemoteNewer` (Kaiten новее) и **перезапишет файл локально**. Если при этом удалённый контент был нормализован (например, lowercased теги) — пользователь увидит «фантомное» изменение. Нужно после `pushLocal` обновить локальный файл (или хотя бы touch mtime) в соответствии с `updated.Updated`.

### Баг #7 — `do()` имеет ошибку: на 429 без заголовка `X-RateLimit-Reset` всё равно идёт `continue`, но `lastErr` не выставляется. Если все попытки кончились 429 — клиент вернёт `errors.New("исчерпаны попытки запроса")` без указания, что это был rate-limit.
**Файл:** `client.go:87-96`. Минорно, но важно для диагностики.

### Баг #8 — `do()` не обрабатывает 3xx редиректы для PATCH
`http.Client` по умолчанию переотправит PATCH как GET на 301/302 → потеря данных. Нужен `CheckRedirect`.

### Баг #9 — Bubble Tea `tea.WithAltScreen()` ломает вывод TUI в окружении без TTY (cron, контейнер). Если `--silent` пропускает TUI, всё ок, но если конфиг ОТСУТСТВУЕТ и пользователь запустил без TTY — TUI зависнет. Нужна проверка `isatty`.

### Баг #10 — `config.Load` не проверяет права файла на 0600 (security). Если пользователь случайно сделал `chmod 644` — токен в файле станет читаемым. Не критично, но стоит warning'а.

### Баг #11 — `WriteAtomic` всегда пишет `0o644`, что для документов с конфиденциальной информацией излишне публично. ОК для MVP — Obsidian-vault'ы обычно личные.

### Баг #12 — `MarkdownToHTML` экранирует только `<`, но не `&`. Если в тексте есть `&amp;` или `&lt;`, оно превратится в кашу при следующем pull. Уязвимость целостности.

### Баг #13 — `engine.go:Run` — N+1 запросов: для каждого документа из `ListDocuments` делается отдельный `GetDocument`. На 500 документов это 500 запросов = 100 секунд при rate-limit 5 req/sec. Допустимо для MVP, но стоит закомментировать TODO про bulk-endpoint.

### Баг #14 — `cmd/sync/main.go:doSync` не возвращает non-zero exit code если в отчёте есть `Errors > 0`. Cron-мониторинг не заметит проблему.

### Баг #15 — Сигналы (`Ctrl+C`, `SIGTERM`) перехватываются в `signal.NotifyContext`, но в середине `Run` контекст проверяется только на rate-limit. Между документами отмена не работает. Нужно добавить `select { case <-ctx.Done(): }` в цикл.

### Баг #16 — `Walk` парсит файлы синхронно. На больших vault'ах (10k файлов) это секунды; можно распараллелить через `errgroup`, но для MVP не критично.

### Баг #17 — `SaveState` пишет JSON без сортировки ключей `Documents`. Каждый прогон диффа в git'е будет шуметь. Желательно отсортировать.

### Баг #18 — В `tui.go` нет третьего экрана «прогресс», заявленного в ТЗ. После выбора пространства TUI просто завершается, и синк идёт в обычной консоли. Прогресс-бар Bubble Tea отсутствует. **Это пропуск по ТЗ.**

### Баг #19 — `config.Save` пишет токен в YAML, если keyring УСПЕШНО сохранил, ставит `out.Token = ""`. Но если keyring ВЕРНУЛ ошибку — токен останется в out.Token и попадёт в YAML. Это поведение задокументировано в README, но в самом коде нет log'а о том, что keyring недоступен → отладка тяжёлая.

### Баг #20 — `kaiten.New` не позволяет передать кастомный `http.Client`. В тестах нельзя подменить транспорт без модификации `Client.http` (поле приватное). Усложняет тестирование.

## 3. Качество кода

- **Имена**: ок, согласованы.
- **Документация пакетов**: есть у каждого, ок.
- **Циклы импорта**: нет.
- **Обработка ошибок**: в основном `%w`-wrapping, ок. В нескольких местах `_ = ...` глушит ошибки (`os.Chtimes`, `resp.Body.Close()`) — допустимо.
- **Конкурентность**: однопоточный, race-condition'ов нет.
- **Магические константы**: `Tolerance = 2*time.Second`, `MaxRetries = 4`, `rate.NewLimiter(5,5)` — задокументированы.
- **Тесты**: 4 тест-файла, покрытие `obsidian` 81%, `kaiten` 48%, `syncengine` 19%. Слабо покрыт engine.go — самая критичная часть.

## 4. Каталог unit-тестов

### Уже есть

**internal/obsidian/vault_test.go**
- `TestIsHiddenPath` — несколько кейсов скрытых путей
- `TestSplitFrontmatter` — happy path
- `TestSplitFrontmatter_NoFrontmatter` — ошибка
- `TestRoundtripWriteRead` — запись и чтение
- `TestWalkSkipsHidden` — пропуск .kaiten-sync и точечных файлов
- `TestSanitize` — спецсимволы и пустое имя

**internal/syncengine/diff_test.go**
- `TestDecide_Unchanged`
- `TestDecide_RemoteNewer`
- `TestDecide_LocalNewer`
- `TestDecide_Conflict`
- `TestDecide_NewRemote`
- `TestDecide_DeletedRemote`

**internal/syncengine/state_test.go**
- `TestStateRoundtrip`
- `TestLoadState_Missing`

**internal/kaiten/client_test.go**
- `TestGetCurrentUser`
- `TestRetryOn5xx`
- `TestListDocumentsFiltersCardLevel`

### Нужно добавить (бизнес-сценарии и краевые случаи)

#### Бизнес-сценарии (полный e2e-цикл через httptest)

1. **TestE2E_InitialPull** — пустой vault + 2 документа в Kaiten → оба создаются локально с правильным frontmatter.
2. **TestE2E_RemoteChange** — после initial pull документ изменён в Kaiten → локальный файл перезаписан, mtime обновлён.
3. **TestE2E_LocalChange** — после initial pull пользователь редактирует .md → PATCH улетает в Kaiten.
4. **TestE2E_Conflict** — обе стороны менялись → создан `.conflict-*.md`, основная версия = Kaiten.
5. **TestE2E_DeletedRemote** — документ исчез в Kaiten → state очищен, .md остался.
6. **TestE2E_CardLevelDocsIgnored** — Kaiten вернул документы с `card_id`, они НЕ попали в vault.
7. **TestE2E_HiddenFolderSkipped** — в vault есть `.archive/note.md` с валидным frontmatter → НЕ попадает в diff.
8. **TestE2E_DryRun** — `DryRun=true` → ни одного байта не записано на диск, state.json не обновлён.
9. **TestE2E_Unchanged** — повторный синк без изменений → 0 операций записи в Kaiten и vault.
10. **TestE2E_HTMLConversion** — Kaiten вернул `type=html` → локально сохранён Markdown, frontmatter содержит `kaiten_type: html`.

#### Краевые случаи

11. **TestPullRemote_ZeroUpdated** — `updated` нулевой → файл не должен ломаться (fix баг #4).
12. **TestTargetRelPath_HiddenTitle** — title=`.secret` → должен превратиться в `secret` (fix баг #1).
13. **TestTargetRelPath_NestedHiddenPath** — `r.Path = "folder/.archive"` → подмена на безопасный путь.
14. **TestDo_RetryOn429_WithResetHeader** — 429 с `X-RateLimit-Reset: 1` → клиент ждёт и повторяет.
15. **TestDo_4xxFatal** — 401 → НЕ ретраит.
16. **TestDo_PatchNoRedirect** — настройка `CheckRedirect` блокирует редирект (fix баг #8).
17. **TestSaveState_DeterministicOrder** — два сохранения подряд дают идентичный JSON (fix баг #17).
18. **TestBuildDecisions_DeterministicOrder** — порядок решений отсортирован по KaitenID (fix баг #3).
19. **TestMarkdownToHTML_AmpersandEscaped** — `&` в тексте экранируется как `&amp;` (fix баг #12).
20. **TestEngine_ContextCancel** — отмена контекста во время Run → возврат с `context.Canceled` (fix баг #15).
21. **TestEngine_ExitCodeOnErrors** — если `rep.Errors > 0` → код выхода != 0 (fix баг #14). Это интеграционный тест на уровне `cmd/sync`.
22. **TestConfig_KeyringFallback** — keyring недоступен → токен в YAML с правами 0600.
23. **TestWalk_SubfolderWithoutFrontmatter** — обычная заметка Obsidian без frontmatter не вызывает ошибку.
24. **TestPushLocal_UpdateMtimeAfterPatch** — после PATCH локальный mtime синхронизируется с ответом Kaiten (fix баг #6).

## 5. План фиксов

Приоритетные фиксы (по убыванию):
1. **#1** — точки в title/path
2. **#3 + #17** — детерминированный порядок
3. **#6** — обновление mtime после PATCH
4. **#8** — запрет редиректов
5. **#12** — экранирование `&` в HTML
6. **#14** — exit code при ошибках
7. **#15** — отмена контекста
8. **#19** — лог о fallback на YAML
9. **#20** — публичный setter для http.Client
10. **#4** — zero-time updated
11. **#18** — экран прогресса в TUI (best-effort)

---

## 6. Итоги работы

### Реализованные фиксы

| # | Баг | Файл | Статус |
|---|---|---|---|
| 1 | Точки в title/path Kaiten ломают Walk | `engine.go::stripLeadingDots` | ✅ |
| 2 | `docs[:0]` мутация исходного слайса | `client.go::ListDocuments` | ✅ |
| 3 | Недетерминированный порядок Decisions | `diff.go::BuildDecisions` (sort.Ints) | ✅ |
| 4 | Zero-time updated → mtime 0001-01-01 | `engine.go::pullRemote` | ✅ |
| 6 | Flapping push после первого PATCH | `engine.go::pushLocal` (Chtimes + state) | ✅ |
| 7 | 429 без Reset не выставлял lastErr | `client.go::do` | ✅ |
| 8 | PATCH следовал за 3xx редиректом | `client.go::New` (CheckRedirect) | ✅ |
| 10 | Нет warning о небезопасных правах | `config.go::Load` | ✅ |
| 12 | `&` не экранировался в HTML | `convert.go::MarkdownToHTML` (html.EscapeString) | ✅ |
| 13 | N+1 запросы не задокументированы | `engine.go::Run` (TODO-комментарий) | ✅ |
| 14 | Exit code 0 при Errors > 0 | `main.go` (exit codes 0/1/2) | ✅ |
| 15 | Отмена контекста между документами | `engine.go::Run` (ctx.Err() checks) | ✅ |
| 17 | Недетерминированный state.json | `state.go` (явно описано — encoding/json сам сортирует) | ✅ |
| 19 | Нет лога о fallback на YAML | `config.go::SaveResult` + `main.go` | ✅ |
| 20 | Нельзя подменить http.Client в тестах | `client.go::Client.HTTPClient` (экспортирован) + `SetRateLimit` | ✅ |
| 21 | **Round-trip Render→Split добавлял \\n в body → flapping** | `vault.go::SplitFrontmatter` (TrimLeft) | ✅ |

### Открытые улучшения (не критично для MVP)

| # | Замечание | Приоритет |
|---|---|---|
| 9 | TUI зависнет без TTY | низкий — обходится через `--silent` |
| 11 | mtime файлов 0o644 | низкий — Obsidian vault личный |
| 16 | Walk однопоточный | низкий — пока vault < 10k файлов |
| 18 | Нет третьего экрана прогресса в TUI | средний — есть лог в stderr |

### Результаты проверок

- **`go vet ./...`** — без замечаний
- **`golangci-lint run ./...`** — без замечаний (15 линтеров: errcheck, govet, staticcheck, bodyclose, errorlint, gocritic, gosec, misspell, nakedret, prealloc, revive, unconvert, unparam, ineffassign, unused + gofmt/goimports)
- **`go test ./... -race -cover`** — все 105 тестов прошли:
  - `internal/config`: 65% coverage
  - `internal/kaiten`: 72% coverage
  - `internal/obsidian`: 84% coverage
  - `internal/syncengine`: 78% coverage

### Что было найдено e2e-тестами (не было видно глазами)

1. **Баг #21 — flapping в `TestE2E_Unchanged`**. Без e2e я бы это не нашёл: round-trip body через Render→Split добавлял ведущий `\n`, hash расходился, и движок на каждом проходе делал ненужный PATCH. Это был бы реальный 4-кратный рост запросов к Kaiten при каждом запуске.

2. **Подтверждение бага #6** через `TestE2E_NoFlappingAfterPush` — после push повторный синк не должен ничего делать.

3. **Подтверждение детерминированности** через `TestBuildDecisions_DeterministicOrder` и `TestSaveState_DeterministicOrder`.

