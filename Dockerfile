FROM --platform=$BUILDPLATFORM golang:alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o /out/youkids ./cmd/youkids

FROM alpine:3.21

LABEL org.opencontainers.image.source="https://github.com/catdevman/youkids" \
      org.opencontainers.image.description="Parent-curated YouTube sidecar for Plex"

RUN apk add --no-cache \
    ca-certificates \
    curl \
    ffmpeg \
    python3 \
    tzdata \
    && curl -fsSL https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp -o /usr/local/bin/yt-dlp \
    && chmod a+rx /usr/local/bin/yt-dlp

COPY --from=builder /out/youkids /usr/local/bin/youkids

ENV YOUKIDS_ADDR=:8090 \
    YOUKIDS_DATA_DIR=/data \
    YOUKIDS_MEDIA_DIR=/media \
    YOUKIDS_SYNC_CRON="0 0 * * *"

VOLUME ["/data", "/media"]
EXPOSE 8090

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8090/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/youkids"]
