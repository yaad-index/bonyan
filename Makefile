# One-stop verification aliases. Run `make check` before pushing; CI runs the
# same target. `make install-hooks` wires githook-check (fmt, vet, lint, tidy;
# no build or test) in as a pre-commit hook. Opt-in: it changes git config.
#
# Every dev tool is tracked in tools/go.mod and run through `go tool`, so
# nothing has to be installed and nothing lands in the library's own go.mod.

TOOL         := go tool -modfile=$(CURDIR)/tools/go.mod
LOCAL_PREFIX := github.com/yaad-index/bonyan
HOOK_PATH    := .githooks

# Nested modules (ADR 0002). `./...` in the root never enters them, so every
# target that works per module runs in each of these as well. Formatting walks
# the whole tree and covers them already.
NESTED       := memory/sqlite memory/honcho tokenize/tiktoken

.PHONY: help fmt fmt-check lint vet test build tidy-check release-check githook-check check install-hooks

help:
	@echo "Targets:"
	@echo "  fmt            format Go files and tidy every module in place (gofumpt + goimports + go mod tidy)"
	@echo "  fmt-check      verify formatting without changes (fails if dirty)"
	@echo "  lint           run golangci-lint on all packages, nested modules included"
	@echo "  vet            go vet ./... in the root and each nested module"
	@echo "  test           go test -race -timeout 2m ./... in the root and each nested module"
	@echo "  build          go build ./... in the root and each nested module"
	@echo "  tidy-check     verify go.mod / go.sum are tidy in every module (go mod tidy -diff)"
	@echo "  release-check  refuse a nested module whose release version is set while it still requires the placeholder root"
	@echo "  githook-check  fmt-check + vet + lint + tidy-check (what the pre-commit hook runs)"
	@echo "  check          full CI chain: vet + build + test + fmt-check + lint + tidy-check + release-check"
	@echo "  install-hooks  generate .githooks/pre-commit and point git core.hooksPath at it"

fmt:
	$(TOOL) gofumpt -w .
	$(TOOL) goimports -w -local $(LOCAL_PREFIX) .
	go mod tidy
	cd tools && go mod tidy
	for m in $(NESTED); do (cd $$m && go mod tidy) || exit 1; done

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
	for m in $(NESTED); do (cd $$m && $(TOOL) golangci-lint run ./...) || exit 1; done

vet:
	go vet ./...
	for m in $(NESTED); do (cd $$m && go vet ./...) || exit 1; done

test:
	go test -race -timeout 2m ./...
	for m in $(NESTED); do (cd $$m && go test -race -timeout 2m ./...) || exit 1; done

build:
	go build ./...
	for m in $(NESTED); do (cd $$m && go build ./...) || exit 1; done

tidy-check:
	go mod tidy -diff
	cd tools && go mod tidy -diff
	for m in $(NESTED); do (cd $$m && go mod tidy -diff) || exit 1; done

# A nested module must not be released while it still requires the root at the
# placeholder version it is developed against (ADR 0002, section 5). The check
# is a Go program in the tools module, so it needs nothing besides Go.
release-check:
	cd tools && go run ./releasecheck .. $(NESTED)

githook-check: fmt-check vet lint tidy-check

check: vet build test fmt-check lint tidy-check release-check

install-hooks:
	@mkdir -p $(HOOK_PATH)
	@printf '#!/bin/sh\nexec make githook-check\n' > $(HOOK_PATH)/pre-commit
	@chmod +x $(HOOK_PATH)/pre-commit
	@git config core.hooksPath $(HOOK_PATH)
	@echo "installed $(HOOK_PATH)/pre-commit; git core.hooksPath -> $(HOOK_PATH)"
