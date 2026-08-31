COMPOSE := podman compose -f deploy/compose.yaml
DATABASE_URL ?= postgres://agentfleet:agentfleet@localhost:5433/agentfleet?sslmode=disable

.PHONY: up down logs build test check migrate-up migrate-down

up:
	$(COMPOSE) up -d --build

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

# deploy/compose.yaml already applies migrations via its one-shot `migrate`
# service on `make up`; these targets are for running them directly against
# DATABASE_URL when you have golang-migrate installed locally.
migrate-up:
	migrate -database "$(DATABASE_URL)" -path deploy/migrations up

migrate-down:
	migrate -database "$(DATABASE_URL)" -path deploy/migrations down 1
