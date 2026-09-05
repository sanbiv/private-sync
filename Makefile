BINARY := private-sync
PKG    := ./cmd/private-sync

.PHONY: build install test vet lint run clean

build:
	go build -o bin/$(BINARY) $(PKG)

install:
	go install $(PKG)

test:
	go test ./...

vet:
	go vet ./...

run: build
	./bin/$(BINARY)

clean:
	rm -rf bin
