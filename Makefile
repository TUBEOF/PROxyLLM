BINARY := proxyllm

# Go packages scoped to the module's own code — `./...` would descend into
# webui/node_modules (some npm packages ship Go sources) and break
# vet/test/lint runs. The explicit list is also what CI uses.
GO_PKGS := ./cmd/... ./internal/... ./webui

.PHONY: build test test-race lint fmt vet run clean docker webui-install webui-build webui-lint webui-typecheck webui-assets

build:
	go build -trimpath -o $(BINARY) ./cmd/proxyllm

test:
	go test $(GO_PKGS)

test-race:
	go test -race $(GO_PKGS)

lint:
	golangci-lint run

fmt:
	gofmt -w $(GO_PKGS)

vet:
	go vet $(GO_PKGS)

run: build
	./$(BINARY)

clean:
	rm -f $(BINARY) $(BINARY).exe

# Development: build the image from source and run the deployment compose
# file against it (IMAGE overrides the pinned registry image).
docker:
	docker build -f docker/Dockerfile -t proxyllm:dev .
	IMAGE=proxyllm:dev docker compose -f docker/compose.yaml up

# --- WebUI (plan §9; dist is embedded via webui/embed.go) --------------

webui-install:
	cd webui && npm ci

# Builds the SPA into webui/dist (replacing the committed placeholder
# index.html), so a subsequent `make build` embeds the real assets.
webui-build:
	cd webui && npm run build

webui-lint:
	cd webui && npm run lint

webui-typecheck:
	cd webui && npm run typecheck

# Full asset pipeline: install (when node_modules is absent), build, then
# embed via the normal Go build.
webui-assets: webui-install webui-build
	@echo "WebUI assets built into webui/dist — run 'make build' to embed them into the binary."