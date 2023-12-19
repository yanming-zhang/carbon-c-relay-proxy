FROM golang:alpine as builder

WORKDIR /Users/rocketzhang/go/src/git.xxx.com/sre/carbon-c-relay-proxy
COPY . .

RUN GOPROXY=https://goproxy.cn CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build

FROM alpine:latest

WORKDIR /usr/bin
COPY --from=builder /Users/rocketzhang/go/src/git.xxx.com/sre/carbon-c-relay-proxy/carbon-c-relay-proxy .
RUN chmod +x /usr/bin/carbon-c-relay-proxy

ENTRYPOINT ["carbon-c-relay-proxy"]

