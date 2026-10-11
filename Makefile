SHELL := /bin/sh

GO ?= go
GOCACHE ?= /tmp/llm-local-service-go-cache
GO_CMD = GOCACHE=$(GOCACHE) $(GO)
LOAD_ENV = set -a; [ ! -f .env ] || . ./.env; set +a;
APP_NAME ?= server
BIN_DIR ?= bin
IMAGE ?= llm-local-service:dev

.PHONY: build test run check docker-build compose-up smoke load

build:
	mkdir -p $(BIN_DIR)
	$(GO_CMD) build -trimpath -o $(BIN_DIR)/$(APP_NAME) ./cmd/server

test:
	$(GO_CMD) test ./...

run:
	$(LOAD_ENV) $(GO_CMD) run ./cmd/server

check:
	@files="$$(find . -type f -name '*.go' -not -path './.git/*' -print)"; \
	if [ -n "$$files" ]; then \
		test -z "$$(gofmt -l $$files)" || { echo 'gofmt required'; exit 1; }; \
		$(GO_CMD) vet ./...; \
	fi
	$(MAKE) test

docker-build:
	docker build --tag $(IMAGE) .

compose-up:
	docker compose up -d --build

smoke:
	$(LOAD_ENV) ./scripts/smoke.sh

load:
	$(LOAD_ENV) ./scripts/load.sh
