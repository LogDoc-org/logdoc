BINARY  := logdoc
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build run test lint vet up down clean ui

ui:
	cd ui && npm install && npm run build

build:
	go build -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/logdoc

run: build
	./bin/$(BINARY)

test:
	go test ./...

vet:
	go vet ./...

lint: vet
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run ./... || echo "golangci-lint не установлен — выполнен только go vet"

up:
	docker compose -f deploy/docker-compose.dev.yml up -d

down:
	docker compose -f deploy/docker-compose.dev.yml down

clean:
	rm -rf bin
