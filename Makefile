# forge — local-first CI/CD runner.
#
# The common path is `make` (build everything) and `make ci` (everything the
# project checks). Nothing here reaches the network except dependency downloads.

SHELL := /bin/bash
.DEFAULT_GOAL := all

BINARY      := forge
BIN_DIR     := bin
CMD         := ./cmd/forge
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X github.com/nickcross-79/forge/internal/cli.buildVersion=$(VERSION)
GO_PACKAGES := ./...

# CGO is off deliberately for builds: the SQLite driver is pure Go, so the
# binary builds and cross-compiles with no C toolchain anywhere in the picture.
#
# The race detector is the one exception — it requires cgo — so the test targets
# re-enable it. That does not change what is linked into the shipped binary.
export CGO_ENABLED := 0

NPM := npm --prefix web

.PHONY: help
help: ## Show this help
	@echo "forge — make targets"
	@echo
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
	@echo
	@echo "Typical use:"
	@echo "  make            build the dashboard and the binary"
	@echo "  make ci         lint, test and build everything"
	@echo "  make dev        run the API and the Vite dev server together"

.PHONY: all
all: web build ## Build the dashboard, then the binary that embeds it

# ---------- Go ----------

.PHONY: build
build: ## Build the forge binary into bin/
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) $(CMD)
	@echo "built $(BIN_DIR)/$(BINARY) ($(VERSION))"

.PHONY: install
install: ## Install forge into GOPATH/bin
	go install -trimpath -ldflags "$(LDFLAGS)" $(CMD)

.PHONY: test
test: ## Run the Go test suite with the race detector
	CGO_ENABLED=1 go test -race -timeout 300s $(GO_PACKAGES)

.PHONY: test-short
test-short: ## Run the Go tests, skipping the slow ones
	go test -short -timeout 120s $(GO_PACKAGES)

.PHONY: cover
cover: ## Run tests and open a coverage report
	CGO_ENABLED=1 go test -race -coverprofile=coverage.out -covermode=atomic $(GO_PACKAGES)
	go tool cover -func=coverage.out | tail -1
	@echo "run: go tool cover -html=coverage.out"

.PHONY: lint
lint: ## Run golangci-lint (falls back to go vet if not installed)
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not found; running go vet instead"; \
		go vet $(GO_PACKAGES); \
	fi

.PHONY: fmt
fmt: ## Format Go sources
	gofmt -w $$(git ls-files '*.go' 2>/dev/null || find . -name '*.go' -not -path './web/*')

.PHONY: tidy
tidy: ## Tidy go.mod
	go mod tidy

# ---------- frontend ----------

web/node_modules: web/package.json
	$(NPM) install
	@touch web/node_modules

.PHONY: web-deps
web-deps: web/node_modules ## Install frontend dependencies

.PHONY: web
web: web-deps ## Build the dashboard into web/dist (embedded by `make build`)
	$(NPM) run build
	# Vite empties dist/ on every build, which would remove the placeholder that
	# keeps the directory in git. Without it, `go:embed all:dist` fails to
	# compile in a fresh clone that has not built the frontend yet.
	@touch web/dist/.gitkeep

.PHONY: web-test
web-test: web-deps ## Run the frontend unit tests
	$(NPM) run test

.PHONY: web-typecheck
web-typecheck: web-deps ## Type-check the dashboard
	$(NPM) run typecheck

# ---------- developing ----------

.PHONY: dev
dev: web-deps ## Run the API and the Vite dev server side by side
	@echo "API      http://127.0.0.1:7777"
	@echo "Dev UI   http://127.0.0.1:5173  (proxies /api to the API)"
	@echo
	@trap 'kill 0' EXIT INT TERM; \
	go run $(CMD) serve & \
	$(NPM) run dev & \
	wait

.PHONY: run
run: build ## Build, then run the example pipeline
	$(BIN_DIR)/$(BINARY) run examples/basic.yml

.PHONY: serve
serve: all ## Build everything, then serve the dashboard
	$(BIN_DIR)/$(BINARY) serve --open

# ---------- checks ----------

.PHONY: ci
ci: lint test web-typecheck web-test web build ## Everything the project checks
	@echo
	@echo "all checks passed"

.PHONY: e2e
e2e: build ## Exercise the CLI end to end in a scratch directory
	@bash scripts/e2e.sh $(CURDIR)/$(BIN_DIR)/$(BINARY)

# ---------- housekeeping ----------

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN_DIR) web/dist coverage.out
	@mkdir -p web/dist && touch web/dist/.gitkeep

.PHONY: clean-all
clean-all: clean ## Also remove dependencies and local forge state
	rm -rf web/node_modules .forge
