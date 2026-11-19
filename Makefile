.PHONY: test test-mcp-stand test-v1-acceptance run fmt vet web-dev web-build check
run:
	go run ./cmd/regate -config ./examples/regate.hcl -state ./.state

test:
	go test ./...

test-mcp-stand:
	go test -race ./internal/server -run TestTwoProcessMCP -count=1 -v

test-v1-acceptance:
	go test -race ./internal/server -run '^TestV1Acceptance$$' -count=1 -v

fmt:
	gofmt -w ./cmd ./internal

vet:
	go vet ./...

web-dev:
	cd web && npm install && npm run dev

web-build:
	cd web && npm install && npm run build

check: fmt vet test
