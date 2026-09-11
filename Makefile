# FuelMind Makefile
# Windows users without `make`: just use the go commands directly. CI runs them too.

GO         ?= go
GOFLAGS    ?= -trimpath
LDFLAGS    ?= -s -w
BIN_DIR    ?= dist
BIN_NAME   ?= fuelmind-core
PKG        := ./...

.PHONY: help build test run tidy vet fmt clean windows coverage

help: ## Show this help
	@for f in $(MAKEFILE_LIST); do \
		grep -E '^[a-zA-Z_-]+:.*?## .*$$' $$f | awk 'BEGIN{FS=":.*?## "}{printf "  %-12s %s\n", $$1, $$2}'; \
	done

build: ## Build the local core for the host OS
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BIN_NAME) ./cmd/fuelmind-core

test: ## Run all tests with race detector
	$(GO) test -race -count=1 $(PKG)

run: ## Run the local core on the host
	$(GO) run ./cmd/fuelmind-core

tidy: ## go mod tidy
	$(GO) mod tidy

vet: ## go vet
	$(GO) vet $(PKG)

fmt: ## gofmt -s on all files (write mode)
	$(GO) fmt $(PKG)
	@gofmt -s -w .

windows: ## Cross-compile a Windows .exe into dist/
	@mkdir -p $(BIN_DIR)
	GOOS=windows GOARCH=amd64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BIN_NAME).exe ./cmd/fuelmind-core

coverage: ## Run tests and emit an HTML coverage report
	$(GO) test -coverprofile=$(BIN_DIR)/coverage.out $(PKG)
	$(GO) tool cover -html=$(BIN_DIR)/coverage.out -o $(BIN_DIR)/coverage.html

clean: ## Remove dist/
	rm -rf $(BIN_DIR)
