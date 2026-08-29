GO      ?= go
VERSION := $(shell sed -n 's/.*AgentVersion = "\(.*\)"/\1/p' version.go)
OUT_DIR ?= dist
LDFLAGS := -ldflags "-s -w"
TARGETS := linux-amd64 linux-arm64 darwin-arm64

.PHONY: all clean test vet fmt $(TARGETS)

all: $(TARGETS)

linux-amd64:  GOOS := linux
linux-amd64:  GOARCH := amd64
linux-arm64:  GOOS := linux
linux-arm64:  GOARCH := arm64
darwin-arm64: GOOS := darwin
darwin-arm64: GOARCH := arm64

$(TARGETS):
	@mkdir -p $(OUT_DIR)
	@echo "build $@ ($(VERSION))"
	@CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -trimpath $(LDFLAGS) \
		-o $(OUT_DIR)/nexion-remote-agent-$@ .

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w .

test:
	$(GO) test ./...

clean:
	rm -rf $(OUT_DIR)
