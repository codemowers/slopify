# Native device image. Build on the target architecture or with emulation.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/ ./internal/
COPY cmd/player/ ./cmd/player/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/slopify-player ./cmd/player

FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg alsa-utils ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/slopify-player /usr/local/bin/slopify-player
ENTRYPOINT ["slopify-player"]
