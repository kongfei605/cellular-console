GO ?= go

.PHONY: test build build-audio build-uac-ready build-gnss build-native fmt privacy

test:
	$(GO) test -race ./...
	python3 -m unittest discover -s deploy -p 'test_*.py'

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags='-s -w' -o bin/ec20-console-linux-amd64 .

build-audio:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags='-s -w' -o bin/ec20-audio-loopback-linux-amd64 ./cmd/ec20-audio-loopback

build-uac-ready:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags='-s -w' -o bin/ec20-uac-ready-linux-amd64 ./cmd/ec20-uac-ready

fmt:
	$(GO) fmt ./...

build-native:
	$(GO) build -trimpath -o bin/ec20-console .
	$(GO) build -trimpath -o bin/ec20-audio-loopback ./cmd/ec20-audio-loopback

privacy:
	python3 scripts/check_release.py --history

build-gnss:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags='-s -w' -o bin/ec20-gnss-broker-linux-amd64 ./cmd/ec20-gnss-broker
