.PHONY: build build-tagger test test-tagger lint coverage

VERSION  := $(shell cat VERSION.md 2>/dev/null | tr -d '[:space:]')
REPO_URL := $(shell cat REPOSITORY.md 2>/dev/null | tr -d '[:space:]')
LDFLAGS  := -ldflags="-X 'github.com/leqwin/monbooru/internal/web.Version=$(VERSION)' -X 'github.com/leqwin/monbooru/internal/web.RepoURL=$(REPO_URL)'"

build:
	go build $(LDFLAGS) ./cmd/monbooru

build-tagger:
	go build -tags tagger $(LDFLAGS) ./cmd/monbooru

test:
	go test -race ./...

test-tagger:
	go test -tags tagger ./...

lint:
	golangci-lint run

coverage:
	go test -coverprofile=coverage.out $(shell go list ./... | grep -v '/cmd/\|/internal/tagger') 
	go tool cover -html=coverage.out -o coverage.html