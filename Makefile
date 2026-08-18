.DEFAULT_GOAL := help
SHELL := /bin/sh

MODULE  := github.com/zazzool/y-fias
BIN     := bin
GOFLAGS ?=

# Порог регресса по времени выполнения бенчмарков, проценты (§9.3 архитектуры).
BENCH_THRESHOLD ?= 20

.PHONY: help
help: ## Показать этот список
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Собрать все бинарники в bin/
	go build $(GOFLAGS) -o $(BIN)/ ./cmd/...

.PHONY: generate
generate: ## Перегенерировать sql/normalize.sql из Go
	go generate ./...

.PHONY: verify-generate
verify-generate: generate ## Упасть, если sql/normalize.sql устарел (§4.1 архитектуры)
	@git diff --exit-code -- sql/ \
		|| (echo "\nsql/ устарел. Запустите 'make generate' и закоммитьте результат." && exit 1)

.PHONY: test
test: ## Юнит-, property- и golden-тесты (без БД)
	go test $(GOFLAGS) -race ./...

.PHONY: test-int
test-int: ## Интеграционные тесты: PostgreSQL в контейнере, контракт Go<->SQL
	go test $(GOFLAGS) -race -tags=integration ./...

.PHONY: fuzz
fuzz: ## Фаззинг парсера номера дома, 10 минут
	go test -run='^$$' -fuzz=FuzzParseHouse -fuzztime=10m ./internal/parse/

.PHONY: bench
bench: ## Бенчмарки с контролем аллокаций
	go test -run='^$$' -bench=. -benchmem -benchtime=1000x ./...

.PHONY: lint
lint: ## go vet + golangci-lint
	go vet ./...
	golangci-lint run

.PHONY: probe
probe: ## Этап 0.4: замер объёма ГАР (регионы 77, 50)
	go run ./cmd/garprobe -regions=77,50 -gar=testdata/gar

.PHONY: golden
golden: ## Этап 0.2: выгрузить golden-корпус с прода (только SELECT)
	./scripts/dump-golden.sh

.PHONY: replay
replay: ## Прогон golden-корпуса: букв до подсказки, расхождения
	go run ./cmd/yfias-replay -golden=testdata/golden

.PHONY: snapshot
snapshot: ## Собрать снапшот ГАР (регионы 77 и 50, с домами)
	go run ./cmd/yfias-ingest -regions=77,50 -houses -out=data/gar.yfsnap

.PHONY: geo
geo: ## Загрузить границы районов из OpenStreetMap
	go run ./cmd/yfias-geo -regions=77,50 -out=data/geo.json

.PHONY: db-up
db-up: ## Поднять локальный PostgreSQL для тестов
	docker compose up -d --wait postgres

.PHONY: db-down
db-down: ## Остановить локальный PostgreSQL
	docker compose down -v

.PHONY: ci
ci: verify-generate lint test ## Всё, что гоняет CI на каждый push

.PHONY: clean
clean: ## Удалить артефакты сборки
	rm -rf $(BIN) coverage.out
