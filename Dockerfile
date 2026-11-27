FROM golang:1.26.11 AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download github.com/coder/websocket github.com/google/uuid golang.org/x/sys modernc.org/sqlite

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o bin/amux ./cmd \
    && mkdir -p /out/data \
    && chmod 0700 /out/data \
    && chown 65532:65532 /out/data

FROM scratch

COPY --from=builder --chown=65532:65532 /src/bin/amux /usr/local/bin/amux
COPY --from=builder --chown=65532:65532 /out/data /data

USER 65532:65532
WORKDIR /data
VOLUME ["/data"]

ENTRYPOINT ["/usr/local/bin/amux"]
CMD ["server", "--database", "/data/amux.db"]
