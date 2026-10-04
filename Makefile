# Builds jrunner for the current platform (dist/jrunner) and the launcher
# stubs for all platforms (dist/stubs/jrunner-<target>), which "jrunner build"
# needs to create launchers for other operating systems.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/korvin2000/java-runner/internal/version.Tool=$(VERSION)
GOBUILD := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

# GOOS/GOARCH/target
TARGETS := windows/amd64/windows-x64 windows/arm64/windows-aarch64 \
           linux/amd64/linux-x64 linux/arm64/linux-aarch64 \
           darwin/amd64/mac-x64 darwin/arm64/mac-aarch64

ifeq ($(OS),Windows_NT)
EXE := .exe
endif

.PHONY: all build stubs vet clean

all: build stubs

build:
	$(GOBUILD) -o dist/jrunner$(EXE) .

stubs:
	@for t in $(TARGETS); do \
		os=$${t%%/*}; rest=$${t#*/}; arch=$${rest%%/*}; name=$${rest#*/}; ext=; \
		[ "$$os" = windows ] && ext=.exe; \
		echo "stub $$name"; \
		GOOS=$$os GOARCH=$$arch $(GOBUILD) -o dist/stubs/jrunner-$$name$$ext . || exit 1; \
	done

vet:
	go vet ./...

clean:
	rm -rf dist
