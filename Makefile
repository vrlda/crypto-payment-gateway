.PHONY: test vet fmt tidy run

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal pkg

tidy:
	go mod tidy

run:
	go run ./cmd/worker
