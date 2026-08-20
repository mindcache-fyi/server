VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
LDFLAGS  = -ldflags "-s -w -X main.Version=$(VERSION) -X main.GitCommit=$(COMMIT)"

.PHONY: build test lint dev clean swagger release eval

build:
	go build $(LDFLAGS) -o bin/server ./cmd/server

test:
	go test ./... -race -cover

test-short:
	go test ./... -race -short

lint:
	golangci-lint run ./...

AIR ?= $(shell go env GOPATH)/bin/air

dev: swagger
	APP_ENV=development go run ./cmd/server

dev-air: swagger
	APP_ENV=development $(AIR) -c .air.toml

clean:
	rm -rf bin/ dist/

SWAG ?= $(shell go env GOPATH)/bin/swag

swagger:
	$(SWAG) init -g cmd/server/main.go -o docs/ --parseInternal 2>&1 | grep -v "^2.*warning:" || true
	cp docs/swagger.json public/openapi.json

release:
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o dist/server-linux-amd64 ./cmd/server
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o dist/server-linux-arm64 ./cmd/server
	GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o dist/server-darwin-arm64 ./cmd/server

eval:
	go run ./eval
