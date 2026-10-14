# Pinned developer tools, installed into ./bin by `make tools`.
# Bump a version here and re-run `make tools`.

BUF_VERSION                := v1.73.0
GOLANGCI_LINT_VERSION      := v2.14.0
GRPCURL_VERSION            := v1.9.4
PROTOC_GEN_GO_VERSION      := v1.36.12
PROTOC_GEN_GO_GRPC_VERSION := v1.6.2

TOOL_PKGS := \
	github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION) \
	github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) \
	github.com/fullstorydev/grpcurl/cmd/grpcurl@$(GRPCURL_VERSION) \
	google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION) \
	google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)

.PHONY: tools
tools: ## Install pinned buf, golangci-lint, grpcurl, protoc-gen-go and protoc-gen-go-grpc into ./bin
	@mkdir -p $(BIN)
	@for pkg in $(TOOL_PKGS); do \
		echo "go install $$pkg"; \
		GOBIN=$(BIN) go install $$pkg || exit 1; \
	done
