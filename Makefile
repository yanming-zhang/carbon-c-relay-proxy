build:
	mkdir -p bin && cd bin && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ../ && upx carbon-c-relay-proxy

lint:
	golangci-lint run ./...

test: lint
	go test -coverprofile=coverage.out ./...

coverage: lint
	gocov test ./... | gocov-html > coverage.html && open coverage.html
