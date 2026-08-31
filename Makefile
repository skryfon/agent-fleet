COMPOSE := podman compose -f deploy/compose.yaml
DATABASE_URL ?= postgres://agentfleet:agentfleet@localhost:5433/agentfleet?sslmode=disable

.PHONY: up down logs build test check migrate-up migrate-down lint sqlc test-integration e2e runner-image

up:
	$(COMPOSE) up -d --build

# The runner image is not itself a compose service (cmd/supervisor creates
# containers from it dynamically, one per Run, on the `runners` network) —
# build it once before `make up` or a launch fails with a clear
# image-not-found error rather than an implicit pull.
runner-image:
	podman build -f deploy/runner.Dockerfile -t agentfleet-runner .

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f

build:
	go build ./...

test:
	go test ./...

check: build
	go vet ./...
	go test ./...
	golangci-lint run

lint:
	golangci-lint run

# Regenerates internal/store/gen from internal/store/queries/*.sql.
# Requires a live Postgres reachable at sqlc.yaml's database.uri (`make up`
# first) — sqlc's static analyzer cannot resolve every builtin it needs
# (e.g. multi-array unnest()) without a real catalog to check against.
# Commit the result — CI's `sqlc diff` step fails on drift, it does not
# regenerate for you.
sqlc:
	go tool sqlc generate

# Runs tests tagged `integration` against a real Postgres — point
# DATABASE_URL at a scratch database (never one with data you care about;
# these tests migrate up/down and truncate tables).
test-integration:
	go test -tags=integration ./...

# End-to-end suite: real dsh + llm-mock-server/llm-replay against the full
# compose stack (deploy/compose.yaml + deploy/compose.e2e.yaml, profile e2e).
# Not part of `check` — needs a container runtime and is comparatively slow.
e2e:
	go test -tags=e2e ./test/e2e/...

# deploy/compose.yaml already applies migrations via its one-shot `migrate`
# service on `make up`; these targets are for running them directly against
# DATABASE_URL when you have golang-migrate installed locally.
migrate-up:
	migrate -database "$(DATABASE_URL)" -path deploy/migrations up

migrate-down:
	migrate -database "$(DATABASE_URL)" -path deploy/migrations down 1
