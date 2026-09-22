.POSIX:
.PHONY: all build test vet check install uninstall clean fmt dist

PREFIX ?= $(HOME)/.local/bin
DIST   ?= dist

# The binary is statically linked and stdlib-only, so it runs anywhere with no
# Go, no libc and no runtime of any kind. Build once here, copy to machines
# that have no toolchain, and install with `./install.sh --binary <file>`.
PLATFORMS = linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

all: check build

build:
	CGO_ENABLED=0 go build -trimpath -o backcheck .

dist: check
	@rm -rf $(DIST) && mkdir -p $(DIST)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags="-s -w" -o $(DIST)/backcheck-$$os-$$arch . \
			&& printf '  %-28s %s\n' "$(DIST)/backcheck-$$os-$$arch" "$$(du -h $(DIST)/backcheck-$$os-$$arch | cut -f1)"; \
	done
	@echo
	@echo "Copy one to the target machine, then:  ./install.sh --binary ./backcheck-<os>-<arch>"

# What CI and install.sh both run. Keep it green.
check: vet test

vet:
	go vet ./...

test:
	go test ./...

fmt:
	gofmt -l -w *.go

install:
	./install.sh --prefix "$(PREFIX)"

uninstall:
	rm -f "$(PREFIX)/backcheck"

clean:
	rm -rf backcheck $(DIST)
