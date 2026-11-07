# Image of report-service (T-401). Build: docker build -t inspecto/report-service .
# Stage 1 builds a static binary; lint and tests run in CI, not here.
FROM golang:1.27.3-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/report-service ./cmd/report-service

# Stage 2: only the binary, runs as an unprivileged user (SECURITY.md §1). Migrations are
# embedded in the binary; wget is used by the health check.
FROM alpine:3.22.8
RUN addgroup -S -g 10001 inspecto && adduser -S -u 10001 -G inspecto -H inspecto
COPY --from=build /out/report-service /usr/local/bin/report-service
USER 10001:10001
EXPOSE 8083
HEALTHCHECK --interval=10s --timeout=3s --start-period=20s --retries=5   CMD wget -q -O /dev/null http://127.0.0.1:8083/health/live || exit 1
ENTRYPOINT ["/usr/local/bin/report-service"]
