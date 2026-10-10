.PHONY: migrate test test-integration fmt check-fmt generate check-generated vet check check-mod

migrate:
	go run ./cmd/migrate

test:
	go test ./...

test-integration:
	go test -count=1 -tags=integration ./test/integration

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './.git/*')

check-fmt:
	@test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './.git/*'))" || { echo 'Run make fmt'; exit 1; }

generate:
	go tool sqlc generate

check-generated:
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT HUP INT TERM; \
	cp -R internal/store/gen "$$tmp/gen"; \
	go tool sqlc generate && diff -ru "$$tmp/gen" internal/store/gen

vet:
	go vet ./...

check-mod:
	go mod tidy -diff

check: check-mod check-fmt check-generated vet test test-integration
