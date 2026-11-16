# Explicit inputs let Skaffold watch this service independently.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/ ./internal/
COPY server/internal/ ./server/internal/
COPY server/cmd/transcoder/ ./server/cmd/transcoder/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./server/cmd/transcoder

FROM alpine:3.22
RUN apk add --no-cache ffmpeg ca-certificates
COPY --from=build /out/app /app
ENTRYPOINT ["/app"]
