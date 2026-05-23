.PHONY: build run silent test lint fmt install cron-install clean help

BIN      := bin/kaiten-sync
PKG      := ./cmd/sync
VAULT    ?= $(HOME)/Obsidian
CONFIG   ?= $(HOME)/.config/kaiten-sync/config.yaml

help: ## Показать список целей
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS=":.*?## "}; {printf "\033[36m%-15s\033[0m %s\n", $$1, $$2}'

build: ## Сборка бинарника в ./bin/
	@mkdir -p bin
	go build -ldflags="-s -w" -o $(BIN) $(PKG)

run: ## Запуск в интерактивном режиме (VAULT=/path)
	go run $(PKG) --vault $(VAULT)

silent: build ## Тихий синк (для cron)
	./$(BIN) --silent --config $(CONFIG)

test: ## Прогон тестов
	go test ./... -race -cover

lint: ## golangci-lint (требуется установленный golangci-lint 1.59+)
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint не установлен. Установка:"; echo "  curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b \$$(go env GOPATH)/bin v1.61.0"; exit 1; }
	golangci-lint run ./...

fmt: ## gofmt + goimports
	gofmt -w .
	@command -v goimports >/dev/null 2>&1 && goimports -w . || echo "goimports не установлен — пропускаю"

install: build ## Установить в /usr/local/bin
	install -m 0755 $(BIN) /usr/local/bin/kaiten-sync

cron-install: ## Прописать cron каждые 15 минут (macOS/Linux)
	@( crontab -l 2>/dev/null | grep -v 'kaiten-sync --silent' ; \
	   echo "*/15 * * * * /usr/local/bin/kaiten-sync --silent --config $(CONFIG) >> $$HOME/.kaiten-sync/cron.log 2>&1" ) | crontab -
	@echo "Cron установлен. Текущие задачи:"; crontab -l | grep kaiten-sync

cron-uninstall: ## Удалить cron-задачу
	@crontab -l 2>/dev/null | grep -v 'kaiten-sync --silent' | crontab -
	@echo "Cron удалён."

clean: ## Очистить артефакты сборки
	rm -rf bin/ .kaiten-sync/logs/
