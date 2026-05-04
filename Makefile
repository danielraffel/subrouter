export CGO_ENABLED := 0

.PHONY: test run build build-linux accounts mock-upstream

test:
	go test ./...

run:
	go run ./cmd/subrouter serve

build:
	CGO_ENABLED=1 go build -ldflags='-linkmode external' -o bin/subrouter ./cmd/subrouter
	CGO_ENABLED=1 go build -ldflags='-linkmode external' -o bin/mockupstream ./cmd/mockupstream
	codesign -s - -f bin/subrouter
	codesign -s - -f bin/mockupstream

build-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/subrouter-linux-amd64 ./cmd/subrouter

accounts:
	go run ./cmd/subrouter accounts

mock-upstream:
	go run ./cmd/mockupstream
