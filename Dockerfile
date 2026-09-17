# syntax=docker/dockerfile:1

# ---- build stage: compile the API server --------------------------------
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ---- runtime stage: minimal image that serves the API -------------------
FROM alpine:3.21 AS runtime
RUN adduser -D -u 10001 app
USER app
COPY --from=build /out/server /usr/local/bin/server
EXPOSE 8080
ENV PORT=8080
# Give the process enough time to finish active calculations. The container
# runtime's grace period must be longer than this so the application, rather
# than SIGKILL, can report a drain timeout.
ENV SHUTDOWN_TIMEOUT=15s
HEALTHCHECK --interval=1s --timeout=1s --retries=30 \
  CMD wget -q -O - http://127.0.0.1:8080/healthz || exit 1
STOPSIGNAL SIGTERM
ENTRYPOINT ["server"]

# ---- verify stage: one-shot acceptance service --------------------------
# Runs the Go test suite, then the black-box acceptance client against the
# live api service. Exits non-zero if anything fails.
FROM golang:1.25-alpine AS verify
WORKDIR /src
COPY . .
ENV API_URL=http://api:8080
CMD ["sh", "-c", "go test ./... && go run ./cmd/verify"]
