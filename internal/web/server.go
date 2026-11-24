package web

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/catdevman/youkids/internal/db"
	"github.com/catdevman/youkids/internal/notify"
	"github.com/catdevman/youkids/internal/plex"
	"github.com/catdevman/youkids/internal/worker"
	"github.com/catdevman/youkids/internal/ytdlp"
)

//go:embed static/*
var staticFS embed.FS

// Server serves the REST API and embedded parent Web UI.
type Server struct {
	store    *db.Store
	defaults db.Settings
	mediaDir string
	password string
	yt       ytdlp.Client
	plex     *plex.Client
	notifier *notify.Notifier
	worker   *worker.Manager
	logger   *slog.Logger
	mux      *http.ServeMux
}

// NewServer builds the HTTP handler with all API routes and embedded static assets.
func NewServer(
	store *db.Store,
	defaults db.Settings,
	mediaDir string,
	password string,
	yt ytdlp.Client,
	plexClient *plex.Client,
	notifier *notify.Notifier,
	mgr *worker.Manager,
	logger *slog.Logger,
) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		store:    store,
		defaults: defaults,
		mediaDir: mediaDir,
		password: password,
		yt:       yt,
		plex:     plexClient,
		notifier: notifier,
		worker:   mgr,
		logger:   logger,
		mux:      http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func (s *Server) Handler() http.Handler {
	if s.password == "" {
		return s.mux
	}
	return s.basicAuth(s.mux)
}

func (s *Server) basicAuth(next http.Handler) http.Handler {
	expected := []byte(s.password)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		_, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pass), expected) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="YouKids Parent Portal"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Dashboard & sync
	s.mux.HandleFunc("GET /api/status", s.handleStatus)
	s.mux.HandleFunc("POST /api/sync", s.handleSyncAll)

	// Channels
	s.mux.HandleFunc("GET /api/channels", s.handleListChannels)
	s.mux.HandleFunc("POST /api/channels", s.handleCreateChannel)
	s.mux.HandleFunc("PUT /api/channels/{id}", s.handleUpdateChannel)
	s.mux.HandleFunc("DELETE /api/channels/{id}", s.handleDeleteChannel)
	s.mux.HandleFunc("POST /api/channels/{id}/sync", s.handleSyncChannel)

	// Videos
	s.mux.HandleFunc("GET /api/videos", s.handleListVideos)
	s.mux.HandleFunc("POST /api/videos", s.handleAddStandaloneVideo)
	s.mux.HandleFunc("POST /api/videos/approve-all", s.handleApproveAll)
	s.mux.HandleFunc("POST /api/videos/{id}/approve", s.handleApproveVideo)
	s.mux.HandleFunc("POST /api/videos/{id}/reject", s.handleRejectVideo)
	s.mux.HandleFunc("DELETE /api/videos/{id}", s.handleDeleteVideo)

	// Settings & integrations
	s.mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	s.mux.HandleFunc("PUT /api/settings", s.handleSaveSettings)
	s.mux.HandleFunc("POST /api/plex/sections", s.handlePlexSections)
	s.mux.HandleFunc("POST /api/plex/refresh", s.handlePlexRefresh)
	s.mux.HandleFunc("POST /api/plex/apply-labels", s.handlePlexApplyLabels)
	s.mux.HandleFunc("POST /api/notifications/test", s.handleTestNotifications)

	// Embedded UI
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	s.mux.Handle("/", http.FileServer(http.FS(sub)))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.GetStats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var nextSync *time.Time
	if t := s.worker.NextSyncTime(); !t.IsZero() {
		nextSync = &t
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stats":        stats,
		"sync_running": s.worker.IsSyncRunning(),
		"next_sync_at": nextSync,
		"media_dir":    s.mediaDir,
		"storage":      s.worker.GetStorageInfo(r.Context()),
	})
}

func (s *Server) handleSyncAll(w http.ResponseWriter, r *http.Request) {
	summary, err := s.worker.SyncAll(r.Context())
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	channels, err := s.store.ListChannels(r.Context(), false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if channels == nil {
		channels = []db.Channel{}
	}
	writeJSON(w, http.StatusOK, channels)
}

func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Input              string `json:"input"`
		AutoApprove        bool   `json:"auto_approve"`
		SkipShorts         *bool  `json:"skip_shorts"`
		SkipLivestreams    *bool  `json:"skip_livestreams"`
		RetentionDays      *int   `json:"retention_days"`
		RetentionMaxVideos *int   `json:"retention_max_videos"`
		SyncNow            bool   `json:"sync_now"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Input = strings.TrimSpace(req.Input)
	if req.Input == "" {
		writeError(w, http.StatusBadRequest, "channel ID, @handle, or URL is required")
		return
	}

	cfg, _ := s.store.GetSettings(r.Context(), s.defaults)
	skipShorts := cfg.DefaultSkipShorts
	if req.SkipShorts != nil {
		skipShorts = *req.SkipShorts
	}
	skipLive := cfg.DefaultSkipLivestreams
	if req.SkipLivestreams != nil {
		skipLive = *req.SkipLivestreams
	}

	info, err := s.yt.ResolveChannel(r.Context(), req.Input)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not resolve YouTube channel: "+err.Error())
		return
	}

	ch := &db.Channel{
		YouTubeID:          info.YouTubeID,
		Name:               info.Name,
		URL:                info.URL,
		AvatarURL:          info.AvatarURL,
		AutoApprove:        req.AutoApprove,
		Enabled:            true,
		SkipShorts:         skipShorts,
		SkipLivestreams:    skipLive,
		RetentionDays:      req.RetentionDays,
		RetentionMaxVideos: req.RetentionMaxVideos,
	}
	if err := s.store.CreateChannel(r.Context(), ch); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			writeError(w, http.StatusConflict, "this channel is already added")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var syncSummary *worker.SyncSummary
	if req.SyncNow {
		syncSummary, _ = s.worker.SyncChannel(r.Context(), ch.ID)
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"channel": ch,
		"sync":    syncSummary,
	})
}

func (s *Server) handleUpdateChannel(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid channel id")
		return
	}
	ch, err := s.store.GetChannel(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "channel not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var req struct {
		Name               *string `json:"name"`
		AutoApprove        *bool   `json:"auto_approve"`
		Enabled            *bool   `json:"enabled"`
		SkipShorts         *bool   `json:"skip_shorts"`
		SkipLivestreams    *bool   `json:"skip_livestreams"`
		RetentionDays      *int    `json:"retention_days"`
		RetentionMaxVideos *int    `json:"retention_max_videos"`
		ClearRetention     bool    `json:"clear_retention"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
		ch.Name = strings.TrimSpace(*req.Name)
	}
	if req.AutoApprove != nil {
		ch.AutoApprove = *req.AutoApprove
	}
	if req.Enabled != nil {
		ch.Enabled = *req.Enabled
	}
	if req.SkipShorts != nil {
		ch.SkipShorts = *req.SkipShorts
	}
	if req.SkipLivestreams != nil {
		ch.SkipLivestreams = *req.SkipLivestreams
	}
	if req.ClearRetention {
		ch.RetentionDays = nil
		ch.RetentionMaxVideos = nil
	} else {
		if req.RetentionDays != nil {
			if *req.RetentionDays <= 0 {
				ch.RetentionDays = nil
			} else {
				ch.RetentionDays = req.RetentionDays
			}
		}
		if req.RetentionMaxVideos != nil {
			if *req.RetentionMaxVideos <= 0 {
				ch.RetentionMaxVideos = nil
			} else {
				ch.RetentionMaxVideos = req.RetentionMaxVideos
			}
		}
	}

	if err := s.store.UpdateChannel(r.Context(), ch); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid channel id")
		return
	}
	if err := s.store.DeleteChannel(r.Context(), id); errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "channel not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleSyncChannel(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid channel id")
		return
	}
	summary, err := s.worker.SyncChannel(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "channel not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) handleListVideos(w http.ResponseWriter, r *http.Request) {
	status := db.VideoStatus(r.URL.Query().Get("status"))
	var channelID *int64
	if rawCh := r.URL.Query().Get("channel_id"); rawCh != "" {
		id, err := parseID(rawCh)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid channel_id")
			return
		}
		channelID = &id
	}
	limit := 100
	if rawLim := r.URL.Query().Get("limit"); rawLim != "" {
		if n, err := strconv.Atoi(rawLim); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	videos, err := s.store.ListVideos(r.Context(), status, channelID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if videos == nil {
		videos = []db.Video{}
	}
	writeJSON(w, http.StatusOK, videos)
}

func (s *Server) handleAddStandaloneVideo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Input string `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Input = strings.TrimSpace(req.Input)
	if req.Input == "" {
		writeError(w, http.StatusBadRequest, "video ID or YouTube URL is required")
		return
	}

	info, err := s.yt.ResolveVideo(r.Context(), req.Input)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not resolve YouTube video: "+err.Error())
		return
	}

	chName := info.ChannelName
	if chName == "" {
		chName = "Standalone"
	}

	existing, err := s.store.GetVideoByYouTubeID(r.Context(), info.YouTubeID)
	if err == nil {
		if existing.Status == db.StatusDownloaded {
			writeJSON(w, http.StatusOK, existing)
			return
		}
		if err := s.store.UpdateVideoStatus(r.Context(), existing.ID, db.StatusApproved, ""); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		existing.Status = db.StatusApproved
		s.worker.TriggerDownload()
		writeJSON(w, http.StatusOK, existing)
		return
	}

	v := &db.Video{
		YouTubeID:       info.YouTubeID,
		ChannelName:     chName,
		Title:           info.Title,
		Description:     info.Description,
		ThumbnailURL:    info.ThumbnailURL,
		DurationSeconds: info.DurationSeconds,
		PublishedAt:     info.PublishedAt,
		IsShort:         info.IsShort,
		IsLivestream:    info.IsLivestream,
		Status:          db.StatusApproved,
	}
	if _, err := s.store.InsertVideoIfNew(r.Context(), v); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.worker.TriggerDownload()
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) handleApproveAll(w http.ResponseWriter, r *http.Request) {
	count, err := s.store.ApproveAllPending(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if count > 0 {
		s.worker.TriggerDownload()
	}
	writeJSON(w, http.StatusOK, map[string]any{"approved": count})
}

func (s *Server) handleApproveVideo(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid video id")
		return
	}
	if err := s.store.UpdateVideoStatus(r.Context(), id, db.StatusApproved, ""); errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "video not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.worker.TriggerDownload()
	writeJSON(w, http.StatusOK, map[string]string{"status": "approved"})
}

func (s *Server) handleRejectVideo(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid video id")
		return
	}
	if err := s.store.UpdateVideoStatus(r.Context(), id, db.StatusRejected, ""); errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "video not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

func (s *Server) handleDeleteVideo(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid video id")
		return
	}
	v, err := s.store.GetVideo(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "video not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	hadFile := v.FilePath != ""
	worker.RemoveVideoFiles(v.FilePath, v.ThumbPath)

	if err := s.store.DeleteVideo(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if hadFile {
		if s.plex != nil {
			if cfg, err := s.store.GetSettings(r.Context(), s.defaults); err == nil {
				_ = s.plex.RefreshLibrary(r.Context(), cfg.PlexURL, cfg.PlexToken, cfg.PlexSectionID)
			}
		}
		// Resume queued downloads if they were paused by the hard storage limit.
		s.worker.TriggerDownload()
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.store.GetSettings(r.Context(), s.defaults)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var cfg db.Settings
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if cfg.SyncCron == "" {
		cfg.SyncCron = "0 0 * * *"
	}
	if err := s.worker.ReloadSchedule(cfg.SyncCron); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if cfg.StorageSoftLimitGB < 0 || cfg.StorageHardLimitGB < 0 {
		writeError(w, http.StatusBadRequest, "storage limits must be 0 or greater")
		return
	}
	if cfg.StorageSoftLimitGB > 0 && cfg.StorageHardLimitGB > 0 && cfg.StorageSoftLimitGB > cfg.StorageHardLimitGB {
		writeError(w, http.StatusBadRequest, "storage soft limit cannot exceed hard limit")
		return
	}
	if cfg.CheckLimitPerChannel <= 0 {
		cfg.CheckLimitPerChannel = 15
	}
	if cfg.MaxResolution <= 0 {
		cfg.MaxResolution = 1080
	}
	if err := s.store.SaveSettings(r.Context(), cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cfg.StorageSoftLimitGB > 0 {
		_, _ = s.worker.PruneAll(r.Context())
	}
	s.worker.TriggerDownload()
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) handlePlexSections(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PlexURL   string `json:"plex_url"`
		PlexToken string `json:"plex_token"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.PlexURL == "" || req.PlexToken == "" {
		cfg, _ := s.store.GetSettings(r.Context(), s.defaults)
		if req.PlexURL == "" {
			req.PlexURL = cfg.PlexURL
		}
		if req.PlexToken == "" {
			req.PlexToken = cfg.PlexToken
		}
	}
	sections, err := s.plex.ListSections(r.Context(), req.PlexURL, req.PlexToken)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sections)
}

func (s *Server) handlePlexRefresh(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.store.GetSettings(r.Context(), s.defaults)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cfg.PlexURL == "" || cfg.PlexToken == "" {
		writeError(w, http.StatusBadRequest, "Plex URL and token are not configured")
		return
	}
	if err := s.plex.RefreshLibrary(r.Context(), cfg.PlexURL, cfg.PlexToken, cfg.PlexSectionID); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "refreshed"})
}

func (s *Server) handlePlexApplyLabels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PlexURL       string `json:"plex_url"`
		PlexToken     string `json:"plex_token"`
		PlexSectionID string `json:"plex_section_id"`
		PlexLabels    string `json:"plex_labels"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	cfg, _ := s.store.GetSettings(r.Context(), s.defaults)
	if req.PlexURL == "" {
		req.PlexURL = cfg.PlexURL
	}
	if req.PlexToken == "" {
		req.PlexToken = cfg.PlexToken
	}
	if req.PlexSectionID == "" {
		req.PlexSectionID = cfg.PlexSectionID
	}
	if req.PlexLabels == "" {
		req.PlexLabels = cfg.PlexLabels
	}

	if req.PlexURL == "" || req.PlexToken == "" || req.PlexSectionID == "" {
		writeError(w, http.StatusBadRequest, "Plex URL, token, and Library Section ID are required")
		return
	}
	labels := plex.ParseLabels(req.PlexLabels)
	if len(labels) == 0 {
		writeError(w, http.StatusBadRequest, "at least one label (e.g. Kids) is required")
		return
	}
	updated, err := s.plex.ApplyLabelsToSection(r.Context(), req.PlexURL, req.PlexToken, req.PlexSectionID, labels)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": updated, "labels": labels})
}

func (s *Server) handleTestNotifications(w http.ResponseWriter, r *http.Request) {
	var cfg db.Settings
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		var err2 error
		cfg, err2 = s.store.GetSettings(r.Context(), s.defaults)
		if err2 != nil {
			writeError(w, http.StatusInternalServerError, err2.Error())
			return
		}
	}
	sent, err := s.notifier.SendTest(r.Context(), cfg)
	if sent == 0 {
		writeError(w, http.StatusBadRequest, "no notification providers are configured")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": sent})
}

func parseID(raw string) (int64, error) {
	return strconv.ParseInt(raw, 10, 64)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
