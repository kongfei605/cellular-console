GO ?= go

.PHONY: test build build-audio build-uac-ready fmt

test:
	$(GO) test -race ./...
	python3 -m unittest discover -s deploy -p 'test_check_uac.py'

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags='-s -w' -o bin/ec20-console-linux-amd64 .

build-audio:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags='-s -w' -o bin/ec20-audio-loopback-linux-amd64 ./cmd/ec20-audio-loopback

build-uac-ready:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags='-s -w' -o bin/ec20-uac-ready-linux-amd64 ./cmd/ec20-uac-ready

fmt:
	$(GO) fmt ./...
