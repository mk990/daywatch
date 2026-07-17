FROM golang:1.25-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/daywatch ./cmd/daywatch

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata wget \
    && addgroup -S daywatch && adduser -S daywatch -G daywatch

COPY --from=build /out/daywatch /usr/local/bin/daywatch

USER daywatch

EXPOSE 2407 8080

HEALTHCHECK --interval=15s --timeout=3s --start-period=10s \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["daywatch"]
