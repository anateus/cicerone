.PHONY: test test-race fmt vet build
test:
	go test ./...
test-race:
	go test -race ./...
fmt:
	gofmt -w $$(find . -name '*.go' -not -path './.git/*')
vet:
	go vet ./...
build:
	go build ./cmd/cicerone
