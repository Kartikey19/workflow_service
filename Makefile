.PHONY: migrate lint-workflows test test-integration fmt check-fmt generate check-generated vet check check-mod

migrate:
	go run ./cmd/api migrate

lint-workflows:
	go test -count=1 -tags=integration ./test/integration -run '^TestWorkflowLint$$'

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

.PHONY: ui-install ui-build ui-check ui-browser
ui-install:
	npm ci --prefix web --no-audit --no-fund
ui-build:
	npm run build --prefix web
ui-check:
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT HUP INT TERM; \
	cp -R web/dist "$$tmp/dist"; \
	npm run build --prefix web && diff -ru "$$tmp/dist" web/dist
ui-browser:
	RUN_UI_TESTS=true go test -count=1 -tags=integration ./test/integration -run '^TestWebUIBrowser$$' -timeout=100s
