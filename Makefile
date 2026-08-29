.PHONY: build build-tagger build-tray test test-tagger lint coverage coverage-tagger

VERSION  := $(shell cat VERSION.md 2>/dev/null | tr -d '[:space:]')
REPO_URL := $(shell cat REPOSITORY.md 2>/dev/null | tr -d '[:space:]')
DOC_URL  := $(shell cat DOC.md 2>/dev/null | tr -d '[:space:]')
LDFLAGS  := -ldflags="-X 'github.com/monbooru/monbooru/internal/web.Version=$(VERSION)' -X 'github.com/monbooru/monbooru/internal/web.RepoURL=$(REPO_URL)' -X 'github.com/monbooru/monbooru/internal/web.DocURL=$(DOC_URL)'"

build:
	go build $(LDFLAGS) ./cmd/monbooru

build-tagger:
	go build -tags tagger $(LDFLAGS) ./cmd/monbooru

build-tray:
	go build -tags tray $(LDFLAGS) ./cmd/monbooru

test:
	go test -race ./...

test-tagger:
	go test -tags tagger -race ./...

lint:
	golangci-lint run

coverage:
	go test -coverprofile=coverage.out $(shell go list ./... | grep -v '/cmd/')
	go tool cover -html=coverage.out -o coverage.html

coverage-tagger:
	go test -tags tagger -coverprofile=coverage-tagger.out $(shell go list ./... | grep -v '/cmd/')
	go tool cover -html=coverage-tagger.out -o coverage-tagger.html