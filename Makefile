GO ?= go

.PHONY: test build fmt

test:
	$(GO) test -race ./...

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags='-s -w' -o bin/ec20-console-linux-amd64 .

fmt:
	$(GO) fmt ./...
