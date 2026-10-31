# Explicit inputs let Skaffold watch this service independently.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/httpx/ ./internal/httpx/
COPY internal/volume/ ./internal/volume/
COPY player/ ./player/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./player

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/app /app
ENTRYPOINT ["/app"]
