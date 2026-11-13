BIN      := bin/kubectl-claimcheck
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X github.com/asaleem9/claimcheck/internal/cli.Version=$(VERSION)

.PHONY: build test test-short golden lint e2e-up e2e e2e-partitions e2e-down demo clean

build: ## Build the kubectl plugin into bin/
	go build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/kubectl-claimcheck

test: ## Unit, golden and agreement tests
	go test ./...

test-short: ## Faster run with fewer random clusters
	go test -short ./...

golden: ## Regenerate golden outputs and scenario snapshots after an intended change
	go test ./internal/explain -run TestScenarios -update

lint:
	golangci-lint run ./...

e2e-up: ## kind cluster on Kubernetes v1.37 with mock GPUs
	hack/kind/up.sh

e2e: ## End-to-end tests against the real scheduler (needs e2e-up)
	go test -tags e2e ./test/e2e -v -count=1

e2e-partitions: ## Same, with each mock GPU split into 4 partitions
	GPU_PARTITIONS=4 hack/kind/up.sh
	go test -tags e2e ./test/e2e -v -count=1 -run TestPartitionScenarios

e2e-down:
	hack/kind/down.sh

demo: build ## Explain a sample scenario offline
	$(BIN) explain pod/llm-train -n ml --from testdata/scenarios/all-in-use/snapshot.yaml

clean:
	rm -rf bin dist
