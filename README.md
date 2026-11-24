# YouKids

A lightweight Go + SQLite + `yt-dlp` sidecar for Plex that gives parents complete control over what YouTube videos their kids can watch—without fighting YouTube Kids' vague age sliders or Family Link account limitations.

## How It Works

1. **Add YouTube channels or individual videos** via the built-in Parent Web UI:
   - **Auto-approve channels**: Every new video from trusted channels is automatically downloaded into Plex.
   - **Review-first channels**: New videos land in the **Review Queue** with thumbnails, duration, and links so you can approve or reject them with one click.
   - **Standalone videos**: Paste any YouTube video ID or URL to immediately approve and download it.
2. **Scheduled checks (default: midnight daily)**: YouKids checks all enabled channels on your cron schedule (`0 0 * * *` by default), filters out YouTube Shorts and livestreams by default, and sends a push notification (ntfy, Discord, Telegram, or webhook) if any new videos are waiting for your review.
3. **Plex "Other Videos" layout**: Videos are downloaded as MP4 with embedded metadata and matching `.jpg` thumbnails:
   ```text
   /media/
     SciShow Kids/
       Why Do Cats Purr [abc123XYZ00].mp4
       Why Do Cats Purr [abc123XYZ00].jpg
   ```
   Point a Plex **Other Videos** library at `/media`, configure your Plex URL & token in Settings, and YouKids automatically triggers a library refresh after downloads or retention pruning.

## Quick Start (Docker Sidecar)

Pre-built multi-arch images (`linux/amd64` and `linux/arm64`) are published automatically to GitHub Container Registry (`ghcr.io/catdevman/youkids:latest`) on every push to `main` and version tag (`v*`).

1. Edit `docker-compose.yml` to set `YOUKIDS_PASSWORD`, `TZ`, and mount your shared Plex kids media directory to `/media`.
2. Start the container:
   ```bash
   docker compose up -d
   ```
   *(Or run `docker compose up -d --build` if you uncomment `build: .` to build locally.)*
3. Open `http://localhost:8090` in your browser.
4. In Plex, create a new **Other Videos** library pointing to the same shared media folder, then open YouKids **Settings → Plex Integration** and click **Load Libraries** to pick it.

## Running Locally (Go)

Requires `go`, `yt-dlp`, and `ffmpeg` on your `PATH`:

```bash
go run ./cmd/youkids
```

## Environment Variables

| Variable | Default | Description |
| --- | --- | --- |
| `YOUKIDS_ADDR` | `:8090` | HTTP listen address |
| `YOUKIDS_DATA_DIR` | `./data` (`/data` in Docker) | Directory for SQLite database (`youkids.db`) |
| `YOUKIDS_MEDIA_DIR` | `./media` (`/media` in Docker) | Output directory mounted into Plex |
| `YOUKIDS_PASSWORD` | *(empty)* | Optional Basic Auth password for the Parent Web UI |
| `YOUKIDS_BASE_URL` | *(empty)* | External URL included in push notifications |
| `YOUKIDS_SYNC_CRON` | `0 0 * * *` | Default cron schedule (editable in Web UI) |
| `YOUKIDS_PLEX_URL` | *(empty)* | Plex server URL, e.g. `http://plex:32400` |
| `YOUKIDS_PLEX_TOKEN` | *(empty)* | Plex `X-Plex-Token` |
| `YOUKIDS_PLEX_SECTION_ID` | *(empty)* | Plex library section ID (blank = refresh all) |
| `YOUKIDS_YTDLP_BIN` | `yt-dlp` | Path to `yt-dlp` binary |
| `YOUKIDS_COOKIES_FILE` | *(empty)* | Optional Netscape cookies file passed to `yt-dlp` |
