# Build a static instigator binary and ship it on scratch. The container
# needs host or macvlan networking to see the client's broadcast BOOTP;
# see the README.
FROM docker.io/library/golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /instigator ./cmd/instigator

FROM scratch
COPY --from=build /instigator /instigator
# media is mounted read-only at /media, config at /etc/instigator.yaml
# CA bundle from the build stage: scratch has none, and an https:// source
# fetch needs it to verify the server's certificate.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
ENTRYPOINT ["/instigator"]
CMD ["serve", "/etc/instigator.yaml"]
