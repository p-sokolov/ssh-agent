.PHONY: build run compose-up compose-down

build:
	go build -o bin/server main.go

run:
	go run main.go

compose-up:
	docker compose up --build -d

compose-down:
	docker compose down