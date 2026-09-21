# ---- build ----
FROM golang:1.25-alpine AS build
WORKDIR /src

# Dependencies first so this layer is cached until go.mod/go.sum change.
# (go.sum is created by `go mod tidy`, run that locally before building.)
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/telnet-central .

# ---- runtime ----
FROM alpine:3.20
RUN adduser -D -H -u 10001 app
USER app
COPY --from=build /out/telnet-central /usr/local/bin/telnet-central

EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1

ENTRYPOINT ["telnet-central"]
