ARG GO_VERSION=1.25
FROM docker.io/library/golang:${GO_VERSION}-alpine AS builder

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY cmd/macvlansync-watcher/main.go main.go
COPY pkg/macvlansync/ pkg/macvlansync/

RUN CGO_ENABLED=0 GOOS=linux go build -a -o macvlansync-watcher main.go

FROM alpine:3.21
WORKDIR /
COPY --from=builder /workspace/macvlansync-watcher .
USER 65532:65532
ENTRYPOINT ["/macvlansync-watcher"]
