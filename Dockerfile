# 单阶段构建，产物只带二进制 + 空 data 目录。
# go 版本必须跟 go.mod 的 `go 1.25.0` 对齐，否则 build 直接报 requires go >= 1.25.0。
FROM golang:1.25-alpine AS build

# 版本号由 CI 注入（docker build --build-arg VERSION=v1.2.3）
ARG VERSION=dev

WORKDIR /src
# 先拷依赖清单，命中缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# 纯 Go（wazero 不需要 CGO），wasm 与面板通过 go:embed 进二进制
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w -X main.appVersion=${VERSION}" \
    -o /out/lovart2api ./cmd/server

FROM alpine:3.20
ARG VERSION=dev
LABEL org.opencontainers.image.title="lovart2api" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/rsfree/loveart2api"
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /out/lovart2api /app/lovart2api
RUN mkdir -p /app/data && chown -R app:app /app
USER app
ENV TZ=Asia/Shanghai
EXPOSE 7864
VOLUME ["/app/data"]
ENTRYPOINT ["/app/lovart2api"]
CMD ["-config", "/app/config.json"]
