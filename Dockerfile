FROM golang:1.25-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /out/idemio ./cmd/idemio && \
    CGO_ENABLED=0 go build -o /out/reconciler ./cmd/reconciler && \
    CGO_ENABLED=0 go build -o /out/relay ./cmd/relay && \
    CGO_ENABLED=0 go build -o /out/fakedownstream ./cmd/fakedownstream && \
    CGO_ENABLED=0 go build -o /out/alertsink ./cmd/alertsink

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/ /usr/local/bin/
COPY manifests/ /etc/idemio/manifests/

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/idemio"]
