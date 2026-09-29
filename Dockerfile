FROM golang:1.25-alpine AS build

RUN apk add --no-cache gcc musl-dev

WORKDIR /src

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
    go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/mollie-bridge .

FROM alpine:3.22

RUN addgroup -S app && \
    adduser -S -G app app && \
    mkdir -p /data && \
    chown -R app:app /data

COPY --from=build /out/mollie-bridge /usr/local/bin/mollie-bridge

USER app

ENTRYPOINT ["/usr/local/bin/mollie-bridge"]
