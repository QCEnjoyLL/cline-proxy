FROM golang:1.26-alpine AS builder

# VERSION 为空时使用源码里的默认版本号（cmd/cline-proxy/version.go）；
# CI 在打 tag 发布时传入 tag 名覆盖它。项目根目录之外没有别的版本来源。
ARG VERSION=""

WORKDIR /build
COPY go.mod ./
RUN go mod download 2>/dev/null || true
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w${VERSION:+ -X main.Version=${VERSION}}" -o cline-proxy ./cmd/cline-proxy
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /build/cline-proxy .

EXPOSE 3457

ENV PORT=3457

ENTRYPOINT ["/app/cline-proxy"]
CMD ["-port", "3457"]
