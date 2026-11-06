GO ?= go

.PHONY: test build build-audio fmt

test:
	$(GO) test -race ./...

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags='-s -w' -o bin/ec20-console-linux-amd64 .

build-audio:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags='-s -w' -o bin/ec20-audio-loopback-linux-amd64 ./cmd/ec20-audio-loopback

fmt:
	$(GO) fmt ./...
