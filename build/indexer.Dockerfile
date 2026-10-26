# Explicit inputs let Skaffold watch this service independently.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/ ./internal/
COPY cmd/indexer/ ./cmd/indexer/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/indexer

FROM alpine:3.22
RUN apk add --no-cache ffmpeg
COPY --from=build /out/app /app
ENTRYPOINT ["/app"]
