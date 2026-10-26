# 单阶段构建，产物只带二进制 + 空 data 目录。
FROM golang:1.22-alpine AS build

WORKDIR /src
# 先拷依赖清单，命中缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# 纯 Go（wazero 不需要 CGO），wasm 与面板通过 go:embed 进二进制
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/lovart2api ./cmd/server

FROM alpine:3.20
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
