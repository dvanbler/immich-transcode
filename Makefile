BIN := immich-transcode
LDFLAGS := -s -w
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64
COMPOSE := docker compose -f test/integration/docker-compose.yml

.PHONY: build test generate dist immich-up immich-down integration

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) .

test:
	go test ./...

# Regenerate the Immich client from api/immich-openapi-specs.json.
generate:
	cd internal/immich/api && go generate ./...

dist:
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "$$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' \
			-o dist/$(BIN)-$$os-$$arch$$ext . || exit 1; \
	done

immich-up:
	$(COMPOSE) up -d --wait

immich-down:
	$(COMPOSE) down -v

# Runs against the throwaway server from `make immich-up` (never a real library).
integration:
	go test -tags integration -count=1 -timeout 15m -v ./test/integration/
