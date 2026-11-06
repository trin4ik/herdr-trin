FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/quota-hub ./cmd/quota-hub

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && mkdir -p /var/lib/quota-hub && chmod 700 /var/lib/quota-hub
COPY --from=build /out/quota-hub /usr/local/bin/quota-hub
# Root reads 0600 host credentials without chmod/chown on the host. Container
# capabilities are dropped by Compose; the root filesystem and sources are RO.
ENTRYPOINT ["/usr/local/bin/quota-hub"]
