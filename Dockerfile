FROM golang:1.26.10-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
# Pure Go (the SQLite driver included), so no C toolchain is needed.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/wa-mcp ./cmd/wa-mcp

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 wa && mkdir /data && chown wa /data
COPY --from=build /out/wa-mcp /usr/local/bin/wa-mcp
USER wa
ENV DATA_DIR=/data LISTEN_ADDR=0.0.0.0:4020
VOLUME /data
EXPOSE 4020
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD wget -qO- http://127.0.0.1:4020/healthz || exit 1
ENTRYPOINT ["wa-mcp"]
CMD ["serve"]
