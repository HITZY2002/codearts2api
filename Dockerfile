FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN go build -o /out/codearts2api ./cmd/server && \
    go build -o /out/codearts2api-login ./cmd/login && \
    go build -o /out/codearts2api-credit ./cmd/credit && \
    go build -o /out/codearts2api-apply ./cmd/apply && \
    go build -o /out/codearts2api-diag ./cmd/diag

FROM alpine:3.20
# 部分网络环境访问 dl-cdn.alpinelinux.org 会超时，可用
#   docker build --build-arg APK_MIRROR=https://mirrors.aliyun.com .
# 换成国内镜像；不传则保持官方源。
ARG APK_MIRROR=""
RUN if [ -n "$APK_MIRROR" ]; then \
        sed -i "s#https://dl-cdn.alpinelinux.org#$APK_MIRROR#g" /etc/apk/repositories; \
    fi && \
    apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/ /usr/local/bin/
COPY config.example.json ./config.example.json
VOLUME ["/app/auths", "/app/data"]
EXPOSE 7866
CMD ["codearts2api", "-config", "config.json"]
