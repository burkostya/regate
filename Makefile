.PHONY: test run fmt vet web-dev web-build check
run:
	go run ./cmd/regate -config ./examples/regate.hcl -state ./.state

test:
	go test ./...

fmt:
	gofmt -w ./cmd ./internal

vet:
	go vet ./...

web-dev:
	cd web && npm install && npm run dev

web-build:
	cd web && npm install && npm run build

check: fmt vet test
