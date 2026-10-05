BIN := immich-transcode
LDFLAGS := -s -w
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64
COMPOSE := docker compose -f test/integration/docker-compose.yml

include api/spec.mk

.PHONY: build test generate spec spec-update dist immich-up immich-down integration

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) .

test:
	go test ./...

# Download the pinned spec ($(IMMICH_VERSION) @ $(IMMICH_COMMIT)) and verify
# its SHA-256 against api/spec.mk.
spec:
	scripts/spec.sh fetch

# Pin a new Immich release: make spec-update IMMICH_VERSION=v3.3.0
# Updates api/spec.mk, the spec, SpecVersion and the integration server image.
spec-update:
	scripts/spec.sh update $(IMMICH_VERSION)

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
