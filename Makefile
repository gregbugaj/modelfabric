BIN      := mfsh
UI_DIR   := web
UI_OUT   := cmd/mfsh/web/dist
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)

.PHONY: all build ui test test-ui check-ui vet fmt check clean install run dist deploy deploy-restart check-install

all: build

## build: compile the node binary (stages the UI first, then embeds it)
#
# Depends on ui so the embedded dashboard is always the one in web/. It used to
# embed "whatever is in $(UI_OUT)", which was survivable while the UI was four
# files and is not now: the dashboard is twenty ES modules and nineteen
# stylesheets, so a build that skipped staging would embed a tree missing
# whichever file was just added, and the browser would ask for it, receive the
# SPA's index.html and fail on "expected a JavaScript module".
build: ui
	go build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/mfsh

## ui: stage the web UI into the embed directory (no bundler, no node_modules)
#
# Copied by glob rather than by name. The dashboard is a set of ES modules and
# a stylesheet that @imports its own parts, so naming four files here meant a
# new module was simply not staged: the browser asked for it, got the SPA's
# index.html, and failed on "expected a JavaScript module". The stale-file
# delete matters for the same reason in reverse — a removed module left behind
# in the embed directory keeps working until someone wonders why.
ui: check-ui
	@mkdir -p $(UI_OUT)/css
	@rm -f $(UI_OUT)/*.js $(UI_OUT)/*.css $(UI_OUT)/css/*.css
	cp $(UI_DIR)/index.html $(UI_DIR)/styles.css $(UI_DIR)/*.js $(UI_OUT)/
	cp $(UI_DIR)/css/*.css $(UI_OUT)/css/
	@rm -f $(UI_OUT)/*.test.mjs
	@echo "staged UI -> $(UI_OUT) ($$(ls $(UI_DIR)/*.js | wc -l) modules, $$(ls $(UI_DIR)/css/*.css | wc -l) stylesheets)"

NODE ?= node

# check-ui: parse the scripts before they are embedded. Staging a file with a
# syntax error yields a blank dashboard and a console message nobody is looking
# at, which has cost real debugging time; node parses these in milliseconds.
# Skipped, with a warning, when no node is on PATH — go build must not need it.
check-ui:
	@if command -v $(NODE) >/dev/null 2>&1; then \
		for f in $(UI_DIR)/*.js; do \
			$(NODE) --input-type=module --check < $$f || { echo "$$f: syntax error"; exit 1; }; \
		done; \
		echo "UI scripts parse ($$(ls $(UI_DIR)/*.js | wc -l) modules)"; \
	else \
		echo "warning: no $(NODE) on PATH, skipping UI syntax check"; \
	fi

## test-ui: unit-test the pure view model (needs node >= 22; set NODE=... if default is older)
test-ui:
	$(NODE) --test $(UI_DIR)/*.test.mjs

## test: unit tests with the race detector
test:
	go test -race ./...

vet:
	go vet ./...

## check: everything CI runs, in the same order. Green here is green there.
check: build vet test check-ui test-ui
	@unformatted="$$(gofmt -l internal cmd)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt would change these files:"; echo "$$unformatted"; exit 1; \
	fi
	@echo "all checks passed"

## check-install: shellcheck-free sanity check of the curl|sh installer
check-install:
	sh -n install.sh && echo "install.sh parses"
	@if command -v shellcheck >/dev/null 2>&1; then shellcheck -s sh install.sh; fi

fmt:
	gofmt -s -w .

## install: build with UI and place the binary on PATH
install: build
	install -m 0755 $(BIN) $(HOME)/.local/bin/$(BIN)

## run: run a node in the foreground with verbose logging
run: build
	./$(BIN) serve -v

## dist: static binaries for the machines in the mesh
dist: ui
	@mkdir -p dist
	GOOS=linux  GOARCH=amd64 CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o dist/$(BIN)-linux-amd64  ./cmd/mfsh
	GOOS=linux  GOARCH=arm64 CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o dist/$(BIN)-linux-arm64  ./cmd/mfsh
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o dist/$(BIN)-darwin-arm64 ./cmd/mfsh
	@cd dist && (sha256sum $(BIN)-* 2>/dev/null || shasum -a 256 $(BIN)-*) > checksums.txt
	@ls -lh dist/

## deploy: stage this build onto the mesh (copies and verifies; never restarts)
deploy: dist
	@sh scripts/deploy.sh $(NODES)

## deploy-restart: stage, then restart each node in turn (skips nodes with models loaded)
deploy-restart: dist
	@sh scripts/deploy.sh -restart $(NODES)

clean:
	rm -rf $(BIN) dist
