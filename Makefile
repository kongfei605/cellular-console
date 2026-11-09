GO ?= go
TARGET_OS ?= linux
TARGET_ARCH ?= amd64

.PHONY: test build build-audio build-uac-ready build-gnss build-native build-mac fmt privacy

test:
	$(GO) test -race ./...
	python3 -m unittest discover -s deploy -p 'test_*.py'

build:
	CGO_ENABLED=0 GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH) $(GO) build -trimpath -ldflags='-s -w' -o bin/ec20-console-$(TARGET_OS)-$(TARGET_ARCH) .

build-audio:
	CGO_ENABLED=0 GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH) $(GO) build -trimpath -ldflags='-s -w' -o bin/ec20-audio-loopback-$(TARGET_OS)-$(TARGET_ARCH) ./cmd/ec20-audio-loopback

build-uac-ready:
	CGO_ENABLED=0 GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH) $(GO) build -trimpath -ldflags='-s -w' -o bin/ec20-uac-ready-$(TARGET_OS)-$(TARGET_ARCH) ./cmd/ec20-uac-ready

fmt:
	$(GO) fmt ./...

build-native:
	$(GO) build -trimpath -o bin/ec20-console .
	$(GO) build -trimpath -o bin/ec20-audio-loopback ./cmd/ec20-audio-loopback

# The Go binaries remain CGO-free. This small helper uses Apple's system SDK.
build-mac: build-native
	@test "$$(uname -s)" = Darwin || (echo 'build-mac requires macOS and Xcode Command Line Tools'; exit 1)
	clang -O2 -Wall -Wextra -Werror -Wno-deprecated-declarations native/darwin/ec20-native.c -framework IOKit -framework CoreFoundation -framework CoreAudio -framework AudioToolbox -Wl,-sectcreate,__TEXT,__info_plist,native/darwin/Info.plist -o bin/ec20-native
	codesign --force --sign - --identifier io.cellularconsole.native bin/ec20-native

privacy:
	python3 scripts/check_release.py --history

build-gnss:
	CGO_ENABLED=0 GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH) $(GO) build -trimpath -ldflags='-s -w' -o bin/ec20-gnss-broker-$(TARGET_OS)-$(TARGET_ARCH) ./cmd/ec20-gnss-broker
