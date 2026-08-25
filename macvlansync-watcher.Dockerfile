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
# Requires root (UID 0) for NET_ADMIN capabilities needed to delete bridge FDB entries.
USER 0:0
ENTRYPOINT ["/macvlansync-watcher"]
