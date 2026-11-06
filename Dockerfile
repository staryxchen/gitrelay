FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gitrelay .

FROM alpine:3.23
RUN apk add --no-cache ca-certificates \
    && addgroup -g 10001 gitrelay \
    && adduser -D -H -u 10001 -G gitrelay gitrelay \
    && mkdir -p /var/cache/gitrelay \
    && chown 10001:10001 /var/cache/gitrelay
COPY --from=build /out/gitrelay /usr/local/bin/gitrelay
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gitrelay"]
