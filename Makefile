.PHONY: test test-mcp-stand test-v1-acceptance test-v1-browser test-v1-full test-v1-soak run fmt vet web-dev web-build check
run:
	go run ./cmd/regate -config ./examples/regate.hcl -state ./.state

test:
	go test ./...

test-mcp-stand:
	go test -race ./internal/server -run TestTwoProcessMCP -count=1 -v

test-v1-acceptance:
	go test -race ./internal/server -run '^TestV1Acceptance$$' -count=1 -v

test-v1-browser:
	cd web && npm run build
	REGATE_V1_ACCEPTANCE_BROWSER=1 go test -race ./internal/server -run '^TestV1Acceptance$$/^browser$$' -count=1 -v

test-v1-full:
	bash scripts/test-v1-full.sh

test-v1-soak:
	bash scripts/test-v1-soak.sh

fmt:
	gofmt -w ./cmd ./internal

vet:
	go vet ./...

web-dev:
	cd web && npm install && npm run dev

web-build:
	cd web && npm install && npm run build

check: fmt vet test
