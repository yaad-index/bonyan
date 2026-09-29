# One-stop verification aliases. Run `make check` before pushing; CI runs the
# same target. `make install-hooks` wires githook-check (fmt, vet, lint, tidy;
# no build or test) in as a pre-commit hook. Opt-in: it changes git config.
#
# Every dev tool is tracked in tools/go.mod and run through `go tool`, so
# nothing has to be installed and nothing lands in the library's own go.mod.

TOOL         := go tool -modfile=tools/go.mod
LOCAL_PREFIX := github.com/yaad-index/bonyan
HOOK_PATH    := .githooks

.PHONY: help fmt fmt-check lint vet test build tidy-check githook-check check install-hooks

help:
	@echo "Targets:"
	@echo "  fmt            format Go files and tidy both modules in place (gofumpt + goimports + go mod tidy)"
	@echo "  fmt-check      verify formatting without changes (fails if dirty)"
	@echo "  lint           run golangci-lint on all packages"
	@echo "  vet            go vet ./..."
	@echo "  test           go test -race -timeout 2m ./..."
	@echo "  build          go build ./..."
	@echo "  tidy-check     verify go.mod / go.sum are tidy in both modules (go mod tidy -diff)"
	@echo "  githook-check  fmt-check + vet + lint + tidy-check (what the pre-commit hook runs)"
	@echo "  check          full CI chain: vet + build + test + fmt-check + lint + tidy-check"
	@echo "  install-hooks  generate .githooks/pre-commit and point git core.hooksPath at it"

fmt:
	$(TOOL) gofumpt -w .
	$(TOOL) goimports -w -local $(LOCAL_PREFIX) .
	go mod tidy
	cd tools && go mod tidy

fmt-check:
	@out=$$($(TOOL) gofumpt -l .); \
	if [ -n "$$out" ]; then \
		echo "gofumpt: the following files need formatting:"; \
		echo "$$out"; \
		exit 1; \
	fi
	@out=$$($(TOOL) goimports -l -local $(LOCAL_PREFIX) .); \
	if [ -n "$$out" ]; then \
		echo "goimports: the following files need import grouping:"; \
		echo "$$out"; \
		exit 1; \
	fi

lint:
	$(TOOL) golangci-lint run ./...

vet:
	go vet ./...

test:
	go test -race -timeout 2m ./...

build:
	go build ./...

tidy-check:
	go mod tidy -diff
	cd tools && go mod tidy -diff

githook-check: fmt-check vet lint tidy-check

check: vet build test fmt-check lint tidy-check

install-hooks:
	@mkdir -p $(HOOK_PATH)
	@printf '#!/bin/sh\nexec make githook-check\n' > $(HOOK_PATH)/pre-commit
	@chmod +x $(HOOK_PATH)/pre-commit
	@git config core.hooksPath $(HOOK_PATH)
	@echo "installed $(HOOK_PATH)/pre-commit; git core.hooksPath -> $(HOOK_PATH)"
