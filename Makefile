VERSION ?= dev
LDFLAGS = -s -w -X main.version=$(VERSION)

build:
	go build -ldflags="$(LDFLAGS)" -o stackscan .

release:
	mkdir -p dist
	GOOS=darwin GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o dist/stackscan-darwin-arm64 .
	GOOS=darwin GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o dist/stackscan-darwin-amd64 .
	GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o dist/stackscan-linux-amd64 .
	GOOS=linux GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o dist/stackscan-linux-arm64 .

.PHONY: build release
