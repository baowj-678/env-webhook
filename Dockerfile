# Build the env-webhook binary
FROM golang:1.20 as builder

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go env -w GOPROXY=https://goproxy.cn,direct
RUN go mod download

# Copy the go source
COPY cmd/ cmd/

# Build
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o env-webhook ./cmd


FROM alpine:latest

# install curl for prestop script
RUN sed -i 's/dl-cdn.alpinelinux.org/mirrors.aliyun.com/g' /etc/apk/repositories
RUN apk --no-cache add curl

WORKDIR /

# install binary
COPY --from=builder /workspace/env-webhook .

# install the prestop script
COPY ./prestop.sh .

USER 65532:65532

ENTRYPOINT ["/env-webhook"]
