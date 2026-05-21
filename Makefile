.PHONY: build test lint vet clean

build:
	go build -o pr-review ./cmd/review

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run

vet:
	go vet ./...

clean:
	rm -f pr-review
