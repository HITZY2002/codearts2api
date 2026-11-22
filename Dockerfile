FROM node:22-alpine AS ui
WORKDIR /ui
COPY web/ui/package.json web/ui/package-lock.json ./
RUN npm ci
COPY web/ui .
RUN npm run build -- --outDir /ui/dist

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN rm -rf /src/internal/server/webui/dist
COPY --from=ui /ui/dist /src/internal/server/webui/dist
RUN CGO_ENABLED=0 go build -o /out/codearts2api ./cmd/server && \
    CGO_ENABLED=0 go build -o /out/codearts2api-login ./cmd/login && \
    CGO_ENABLED=0 go build -o /out/codearts2api-credit ./cmd/credit && \
    CGO_ENABLED=0 go build -o /out/codearts2api-apply ./cmd/apply

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S -g 10001 app && adduser -S -u 10001 -G app app && \
    mkdir -p /app/auths /app/data && chown -R app:app /app
WORKDIR /app
COPY --from=build /out/ /usr/local/bin/
COPY config.example.json ./config.example.json
USER app
VOLUME ["/app/auths", "/app/data"]
EXPOSE 7866
CMD ["codearts2api", "-config", "config.json"]
