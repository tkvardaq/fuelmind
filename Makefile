# FuelMind Makefile. Windows users without `make` can run the go commands
# directly; CI runs the same ones.

GO       ?= go
GOFLAGS  ?= -trimpath
VERSION  ?= dev
LDFLAGS  ?= -s -w -X main.version=$(VERSION)
DIST     ?= dist
PKG      := ./...

.PHONY: help build test test-race lint vet fmt fmt-check run rebuild-mart windows installer clean coverage tidy

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-13s %s\n", $$1, $$2}'

build: ## Build every command for the host OS
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/ ./cmd/...

test: ## Run all tests
	$(GO) test -count=1 $(PKG)

test-race: ## Run all tests with the race detector (needs a C compiler)
	$(GO) test -race -count=1 $(PKG)

vet: ## go vet
	$(GO) vet $(PKG)

fmt: ## Format the tree
	gofmt -s -w .

fmt-check: ## Fail if the tree is not gofmt-clean (what CI runs)
	@out="$$(gofmt -s -l .)"; \
	if [ -n "$$out" ]; then echo "not gofmt-clean:"; echo "$$out"; exit 1; fi

lint: vet fmt-check ## vet + gofmt check

tidy: ## go mod tidy
	$(GO) mod tidy

run: ## Run the core against ~/.fuelmind (or FUELMIND_DATA_DIR)
	$(GO) run ./cmd/fuelmind-core

rebuild-mart: ## Recompute every mart table from transaction history
	$(GO) run ./cmd/fuelmind-core -rebuild-mart

windows: ## Cross-compile the Windows binaries into dist/
	GOOS=windows GOARCH=amd64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/FuelMindCore.exe ./cmd/fuelmind-core
	GOOS=windows GOARCH=amd64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/fuelmind-launcher.exe ./cmd/fuelmind-launcher
	GOOS=windows GOARCH=amd64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/fuelmind-setup.exe ./cmd/fuelmind-setup

installer: ## Build binaries, update artifact and MSI (Windows + WiX v3)
	powershell -ExecutionPolicy Bypass -File installer/build.ps1 -Version $(VERSION)

coverage: ## HTML coverage report
	$(GO) test -coverprofile=$(DIST)/coverage.out $(PKG)
	$(GO) tool cover -html=$(DIST)/coverage.out -o $(DIST)/coverage.html

clean: ## Remove build output
	rm -rf $(DIST)
