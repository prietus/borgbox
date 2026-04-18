BINARY := borgbox
VERSION := 0.5.0
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build clean test run dist

all: build

build:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) .

run: build
	./$(BINARY)

test:
	go vet ./...
	go test ./...

clean:
	rm -rf $(BINARY) dist/

# Cross-compile for all targets Borgbox supports on the server side.
dist: clean
	mkdir -p dist
	GOOS=linux   GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o dist/$(BINARY)-linux-amd64 .
	GOOS=linux   GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o dist/$(BINARY)-linux-arm64 .
	GOOS=darwin  GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o dist/$(BINARY)-darwin-amd64 .
	GOOS=darwin  GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o dist/$(BINARY)-darwin-arm64 .
	cd dist && sha256sum * > SHA256SUMS 2>/dev/null || shasum -a 256 * > SHA256SUMS
	@ls -lh dist/
