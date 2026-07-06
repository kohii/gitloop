.PHONY: build test lint

build:
	go build -o bin/gitloop ./cmd/gitloop

test:
	go test ./...

lint:
	go vet ./...
	@fmtdiff="$$(gofmt -d .)"; \
	if [ -n "$$fmtdiff" ]; then \
		echo "$$fmtdiff"; \
		exit 1; \
	fi
