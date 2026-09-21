FROM golang:1.26-alpine AS development
RUN apk add --no-cache build-base
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

FROM development AS test
RUN apk add --no-cache tor
RUN --mount=type=cache,target=/root/.cache/go-build go vet ./... && go test -race -count=1 ./...

FROM development AS build
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=1 go build -trimpath -ldflags="-s -w -linkmode external -extldflags '-static'" -o /out/torproxy ./cmd/torproxy

FROM alpine:3.24 AS runtime
RUN apk add --no-cache tor ca-certificates \
    && rm -f /usr/share/tor/geoip /usr/share/tor/geoip6 \
    && addgroup -S torproxy && adduser -S -G torproxy torproxy \
    && mkdir -p /data && chown torproxy:torproxy /data
COPY --from=build /out/torproxy /usr/local/bin/torproxy
USER torproxy
ENV TOR_COUNT=10 DATA_DIR=/data
EXPOSE 3128 8080
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/torproxy", "healthcheck"]
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/torproxy"]
