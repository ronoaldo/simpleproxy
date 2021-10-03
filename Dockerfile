FROM golang:1.17 as builder
ADD main.go /tmp
RUN go build -o /tmp/simpleproxy /tmp/main.go

FROM debian:bullseye as runtime
COPY --from=builder /tmp/simpleproxy /usr/local/bin/simpleproxy
RUN apt-get update && apt-get install ca-certificates -yq && apt-get clean
WORKDIR /var/cache/proxy
RUN mkdir -p /var/cache/proxy/cache
ENTRYPOINT ["simpleproxy"]
