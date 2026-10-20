.DEFAULT_GOAL := build

.PHONY: generate deps build run test unit integration up down logs fmt smoke

generate:
	mkdir -p internal/api
	go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 --config api/codegen.yaml api/openapi.yaml

deps: generate
	go mod tidy

build: deps
	mkdir -p bin
	go build -o bin/marketplace ./cmd/server

run: build
	./bin/marketplace

test: deps
	go test -race -count=1 ./...

unit:
	go test -race -count=1 ./internal/money ./internal/rules

integration:
	docker compose up -d db
	docker compose run --rm migrate-test
	docker compose run --rm --build test

up:
	docker compose up --build -d app

down:
	docker compose down

logs:
	docker compose logs -f app

fmt:
	gofmt -w cmd internal

smoke:
	python3 scripts/smoke.py
