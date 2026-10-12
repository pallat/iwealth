.PHONY: test build-pi
test:
	go test ./...
build-pi:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/iwealth-arm64 ./cmd/iwealth
