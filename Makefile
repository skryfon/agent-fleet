COMPOSE := podman compose -f deploy/compose.yaml
DATABASE_URL ?= postgres://agentfleet:agentfleet@localhost:5433/agentfleet?sslmode=disable

.PHONY: up down logs build test migrate-up migrate-down

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

migrate-up:
	migrate -database "$(DATABASE_URL)" -path deploy/migrations up

migrate-down:
	migrate -database "$(DATABASE_URL)" -path deploy/migrations down 1
