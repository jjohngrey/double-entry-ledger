.PHONY: run test migrate

run:
	go run ./cmd/ledger

test:
	go test ./...

migrate:
	migrate -path ./migrations -database "postgres://postgres:postgres@localhost/ledger?sslmode=disable" up