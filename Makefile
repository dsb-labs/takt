# Shortcuts around go commands. The test and dev targets run inside a delegated
# cgroup subtree, which is what lets takt enforce resource limits on exec
# workloads — see the "Delegation" section of docs/operating.md.

.PHONY: test e2e dev lint generate ui dev-ui

test:
	./scripts/delegated.sh go test -short -race ./...

e2e:
	./scripts/delegated.sh go tool gotestsum --format=testname -- -race -timeout 20m ./internal/e2e/...

# The config the dev server reads. Override it for a server that serves another
# file: make dev CONFIG=loadtest/config.toml
CONFIG ?= dev.toml

dev:
	./scripts/delegated.sh go run . serve $(CONFIG)

lint:
	go tool staticcheck ./...

# The web UI bundle the server embeds. This needs node and yarn where the go
# targets do not: a binary built without the bundle still works, and serves a
# page saying the UI is not in the build.
ui:
	yarn --cwd internal/ui/app install --frozen-lockfile
	yarn --cwd internal/ui/app build

# The Vite dev server for working on the UI, which proxies API requests to a
# server started with make dev.
dev-ui:
	yarn --cwd internal/ui/app install --frozen-lockfile
	yarn --cwd internal/ui/app dev

generate:
	go generate ./...
