# Explicit inputs let Skaffold watch this service independently.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/ ./internal/
COPY server/internal/ ./server/internal/
COPY server/cmd/frontend/ ./server/cmd/frontend/
COPY server/web/ ./server/web/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./server/cmd/frontend

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/app /app
ENTRYPOINT ["/app"]
