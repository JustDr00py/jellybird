# Build stage: compile the static Go binary.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/jellybird ./cmd/jellybird

# Runtime stage: static distroless image.
#
# Runs as root by default because jellybird WRITES .strm files into the
# media volume it shares with Jellyfin/Emby/Silo (fresh volumes are
# root-owned). To run unprivileged instead, set `user: "1000:1000"` in
# docker-compose.yml and pre-chown the media/data volumes.
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/jellybird /jellybird
# /media is the library mount point; /data holds the SQLite database.
VOLUME ["/media", "/data"]
ENV JELLYBIRD_DATABASE_PATH=/data/jellybird.db
EXPOSE 8097
ENTRYPOINT ["/jellybird", "-config", "/config.yaml"]
