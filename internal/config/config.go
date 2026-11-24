package config

import (
	"os"
	"path/filepath"
)

// Config holds startup configuration loaded from environment variables.
type Config struct {
	Addr          string
	DataDir       string
	MediaDir      string
	Password      string
	BaseURL       string
	SyncCron      string
	YTDLPBin      string
	CookiesFile   string
	PlexURL       string
	PlexToken     string
	PlexSectionID string
	PlexLabels    string
}

// Load reads configuration from environment variables with sensible defaults.
func Load() Config {
	dataDir := getEnv("YOUKIDS_DATA_DIR", "./data")
	mediaDir := getEnv("YOUKIDS_MEDIA_DIR", "./media")

	return Config{
		Addr:          getEnv("YOUKIDS_ADDR", ":8090"),
		DataDir:       dataDir,
		MediaDir:      mediaDir,
		Password:      os.Getenv("YOUKIDS_PASSWORD"),
		BaseURL:       os.Getenv("YOUKIDS_BASE_URL"),
		SyncCron:      getEnv("YOUKIDS_SYNC_CRON", "0 0 * * *"),
		YTDLPBin:      getEnv("YOUKIDS_YTDLP_BIN", "yt-dlp"),
		CookiesFile:   os.Getenv("YOUKIDS_COOKIES_FILE"),
		PlexURL:       os.Getenv("YOUKIDS_PLEX_URL"),
		PlexToken:     os.Getenv("YOUKIDS_PLEX_TOKEN"),
		PlexSectionID: os.Getenv("YOUKIDS_PLEX_SECTION_ID"),
		PlexLabels:    os.Getenv("YOUKIDS_PLEX_LABELS"),
	}
}

// DBPath returns the SQLite database file path.
func (c Config) DBPath() string {
	return filepath.Join(c.DataDir, "youkids.db")
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
