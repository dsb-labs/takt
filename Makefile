# Shortcuts around go commands. The test and dev targets run inside a delegated
# cgroup subtree, which is what lets orca enforce resource limits on exec
# workloads — see the "Delegation" section of docs/operating.md.

.PHONY: test e2e dev lint generate

test:
	./scripts/delegated.sh go test -short -race ./...

e2e:
	./scripts/delegated.sh go tool gotestsum --format=testname -- -race -timeout 20m ./internal/e2e/...

dev:
	./scripts/delegated.sh go run . serve dev.toml

lint:
	go tool staticcheck ./...

generate:
	go generate ./...
