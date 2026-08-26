FROM docker.io/library/golang:1.25-alpine AS builder
WORKDIR /workspace
COPY cmd/cni-nodad/go.mod cmd/cni-nodad/go.sum ./
RUN go mod download
COPY cmd/cni-nodad/main.go .
RUN CGO_ENABLED=0 GOOS=linux go build -a -o cni-nodad .

FROM alpine:3.21
COPY --from=builder /workspace/cni-nodad /cni-nodad
# Install the binary to the host's CNI bin directory via volume mount.
CMD ["cp", "/cni-nodad", "/opt/cni/bin/"]
