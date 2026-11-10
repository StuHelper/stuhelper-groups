GOWORK ?= off
export GOWORK
.PHONY: setup generate check-generated check test build
setup:
	pnpm install --frozen-lockfile
	pnpm exec lefthook install
generate:
	node scripts/generate.mjs
check-generated:
	node scripts/generate.mjs --check
check: check-generated
	test -n "$(STUHELPER_GROUPS_TEST_DSN)"
	pnpm exec stuhelper-check repository
	node scripts/check-module.mjs
	pnpm exec stuhelper-check docs
	test -z "$$(gofmt -l .)"
	go vet ./...
	go test -race -count=1 ./...
	go build ./...
	pnpm exec stuhelper-check workflows
test:
	test -n "$(STUHELPER_GROUPS_TEST_DSN)"
	go test -race -count=1 ./...
build:
	go build ./...
