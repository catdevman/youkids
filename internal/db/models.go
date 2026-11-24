package db

import "time"

// VideoStatus represents the lifecycle state of a video in YouKids.
type VideoStatus string

const (
	StatusPendingReview VideoStatus = "pending_review"
	StatusApproved      VideoStatus = "approved"
	StatusDownloading   VideoStatus = "downloading"
	StatusDownloaded    VideoStatus = "downloaded"
	StatusRejected      VideoStatus = "rejected"
	StatusFailed        VideoStatus = "failed"
	StatusPruned        VideoStatus = "pruned"
)

// Channel represents a monitored YouTube channel.
type Channel struct {
	ID                 int64      `json:"id"`
	YouTubeID          string     `json:"youtube_id"`
	Name               string     `json:"name"`
	URL                string     `json:"url"`
	AvatarURL          string     `json:"avatar_url"`
	AutoApprove        bool       `json:"auto_approve"`
	Enabled            bool       `json:"enabled"`
	SkipShorts         bool       `json:"skip_shorts"`
	SkipLivestreams    bool       `json:"skip_livestreams"`
	RetentionDays      *int       `json:"retention_days"`
	RetentionMaxVideos *int       `json:"retention_max_videos"`
	LastCheckedAt      *time.Time `json:"last_checked_at"`
	CreatedAt          time.Time  `json:"created_at"`
	// Computed counts populated on list queries
	PendingCount    int `json:"pending_count,omitempty"`
	DownloadedCount int `json:"downloaded_count,omitempty"`
}

// Video represents a discovered or manually added YouTube video.
type Video struct {
	ID              int64       `json:"id"`
	YouTubeID       string      `json:"youtube_id"`
	ChannelID       *int64      `json:"channel_id"`
	ChannelName     string      `json:"channel_name"`
	Title           string      `json:"title"`
	Description     string      `json:"description"`
	ThumbnailURL    string      `json:"thumbnail_url"`
	DurationSeconds int         `json:"duration_seconds"`
	PublishedAt     *time.Time  `json:"published_at"`
	IsShort         bool        `json:"is_short"`
	IsLivestream    bool        `json:"is_livestream"`
	Status          VideoStatus `json:"status"`
	FilePath        string      `json:"file_path"`
	ThumbPath       string      `json:"thumb_path"`
	ErrorMessage    string      `json:"error_message"`
	DownloadedAt    *time.Time  `json:"downloaded_at"`
	CreatedAt       time.Time   `json:"created_at"`
}

// Settings holds runtime-configurable application settings stored in SQLite.
type Settings struct {
	SyncCron                string `json:"sync_cron"`
	CheckLimitPerChannel    int    `json:"check_limit_per_channel"`
	MaxResolution           int    `json:"max_resolution"`
	DefaultSkipShorts       bool   `json:"default_skip_shorts"`
	DefaultSkipLivestreams  bool   `json:"default_skip_livestreams"`
	GlobalRetentionDays     int     `json:"global_retention_days"`
	GlobalRetentionMaxVideo int     `json:"global_retention_max_videos"`
	StorageSoftLimitGB      float64 `json:"storage_soft_limit_gb"`
	StorageHardLimitGB      float64 `json:"storage_hard_limit_gb"`
	BaseURL                 string  `json:"base_url"`

	// Plex integration
	PlexURL       string `json:"plex_url"`
	PlexToken     string `json:"plex_token"`
	PlexSectionID string `json:"plex_section_id"`

	// Push notifications
	NotifyNtfyURL           string `json:"notify_ntfy_url"`
	NotifyNtfyTopic         string `json:"notify_ntfy_topic"`
	NotifyNtfyToken         string `json:"notify_ntfy_token"`
	NotifyDiscordWebhook    string `json:"notify_discord_webhook"`
	NotifyTelegramBotToken  string `json:"notify_telegram_bot_token"`
	NotifyTelegramChatID    string `json:"notify_telegram_chat_id"`
	NotifyGenericWebhookURL string `json:"notify_generic_webhook_url"`
}

// DashboardStats summarizes counts for the UI header and status bar.
type DashboardStats struct {
	PendingReview int `json:"pending_review"`
	Approved      int `json:"approved"`
	Downloading   int `json:"downloading"`
	Downloaded    int `json:"downloaded"`
	Failed        int `json:"failed"`
	Channels      int `json:"channels"`
}
