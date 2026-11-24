package worker

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/catdevman/youkids/internal/db"
	"github.com/catdevman/youkids/internal/notify"
	"github.com/catdevman/youkids/internal/plex"
	"github.com/catdevman/youkids/internal/ytdlp"
)

// SyncSummary reports the outcome of a channel sync run.
type SyncSummary struct {
	ChannelsChecked int      `json:"channels_checked"`
	NewPending      int      `json:"new_pending"`
	NewApproved     int      `json:"new_approved"`
	Skipped         int      `json:"skipped"`
	Pruned          int      `json:"pruned"`
	Errors          []string `json:"errors,omitempty"`
}

// StorageInfo summarizes current media disk usage against configured soft/hard limits.
type StorageInfo struct {
	UsedBytes        int64 `json:"used_bytes"`
	SoftLimitBytes   int64 `json:"soft_limit_bytes"`
	HardLimitBytes   int64 `json:"hard_limit_bytes"`
	HardLimitReached bool  `json:"hard_limit_reached"`
}

// Manager coordinates scheduled channel syncs, background downloads, Plex refreshes, and retention pruning.
type Manager struct {
	store    *db.Store
	defaults db.Settings
	mediaDir string
	yt       ytdlp.Client
	plex     *plex.Client
	notifier *notify.Notifier
	logger   *slog.Logger

	cronMu      sync.Mutex
	cronRunner  *cron.Cron
	cronEntryID cron.EntryID
	cronExpr    string

	syncRunning       atomic.Bool
	hardLimitNotified atomic.Bool
	dlSignal          chan struct{}
	stopCh            chan struct{}
	wg                sync.WaitGroup
}

// NewManager creates a new background worker manager.
func NewManager(
	store *db.Store,
	defaults db.Settings,
	mediaDir string,
	yt ytdlp.Client,
	plexClient *plex.Client,
	notifier *notify.Notifier,
	logger *slog.Logger,
) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		store:    store,
		defaults: defaults,
		mediaDir: mediaDir,
		yt:       yt,
		plex:     plexClient,
		notifier: notifier,
		logger:   logger,
		dlSignal: make(chan struct{}, 1),
		stopCh:   make(chan struct{}),
	}
}

// Start resets any interrupted downloads, starts the download worker, and boots the cron scheduler.
func (m *Manager) Start(ctx context.Context) error {
	if err := m.store.ResetStuckDownloads(ctx); err != nil {
		m.logger.Warn("failed to reset stuck downloads", "err", err)
	}

	cfg, err := m.store.GetSettings(ctx, m.defaults)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}

	m.cronRunner = cron.New()
	if err := m.ReloadSchedule(cfg.SyncCron); err != nil {
		m.logger.Warn("invalid saved cron schedule, falling back to midnight", "cron", cfg.SyncCron, "err", err)
		if err2 := m.ReloadSchedule("0 0 * * *"); err2 != nil {
			return err2
		}
	}
	m.cronRunner.Start()

	m.wg.Add(1)
	go m.downloadLoop()

	// Kick off download worker in case approved items are waiting from before restart.
	m.TriggerDownload()
	return nil
}

// Stop gracefully halts the scheduler and download worker.
func (m *Manager) Stop() {
	m.cronMu.Lock()
	if m.cronRunner != nil {
		ctx := m.cronRunner.Stop()
		<-ctx.Done()
	}
	m.cronMu.Unlock()

	close(m.stopCh)
	m.wg.Wait()
}

// ReloadSchedule validates and updates the active cron schedule.
func (m *Manager) ReloadSchedule(expr string) error {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	if _, err := parser.Parse(expr); err != nil {
		return fmt.Errorf("invalid cron expression %q: %w", expr, err)
	}

	m.cronMu.Lock()
	defer m.cronMu.Unlock()

	if m.cronRunner == nil {
		m.cronExpr = expr
		return nil
	}
	if m.cronEntryID != 0 {
		m.cronRunner.Remove(m.cronEntryID)
	}
	entryID, err := m.cronRunner.AddFunc(expr, func() {
		m.logger.Info("starting scheduled channel sync")
		summary, err := m.SyncAll(context.Background())
		if err != nil {
			m.logger.Error("scheduled sync failed", "err", err)
			return
		}
		m.logger.Info("scheduled sync complete",
			"channels", summary.ChannelsChecked,
			"new_pending", summary.NewPending,
			"new_approved", summary.NewApproved,
			"pruned", summary.Pruned,
		)
	})
	if err != nil {
		return err
	}
	m.cronEntryID = entryID
	m.cronExpr = expr
	return nil
}

// NextSyncTime returns the next scheduled sync time, or zero if not scheduled.
func (m *Manager) NextSyncTime() time.Time {
	m.cronMu.Lock()
	defer m.cronMu.Unlock()
	if m.cronRunner == nil || m.cronEntryID == 0 {
		return time.Time{}
	}
	return m.cronRunner.Entry(m.cronEntryID).Next
}

// IsSyncRunning reports whether a channel sync is currently in progress.
func (m *Manager) IsSyncRunning() bool {
	return m.syncRunning.Load()
}

// TriggerDownload wakes up the background download worker.
func (m *Manager) TriggerDownload() {
	select {
	case m.dlSignal <- struct{}{}:
	default:
	}
}

// SyncAll checks all enabled channels for new videos, sends notifications for pending items,
// queues auto-approved items for download, and applies retention policies.
func (m *Manager) SyncAll(ctx context.Context) (*SyncSummary, error) {
	if !m.syncRunning.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("a sync is already in progress")
	}
	defer m.syncRunning.Store(false)

	cfg, err := m.store.GetSettings(ctx, m.defaults)
	if err != nil {
		return nil, err
	}

	channels, err := m.store.ListChannels(ctx, true)
	if err != nil {
		return nil, err
	}

	summary := &SyncSummary{}
	var newlyPending []db.Video

	for i := range channels {
		ch := &channels[i]
		pending, approved, skipped, err := m.syncOneChannel(ctx, ch, cfg.CheckLimitPerChannel)
		summary.ChannelsChecked++
		summary.NewApproved += approved
		summary.Skipped += skipped
		if err != nil {
			m.logger.Warn("channel sync failed", "channel", ch.Name, "err", err)
			summary.Errors = append(summary.Errors, fmt.Sprintf("%s: %v", ch.Name, err))
			continue
		}
		newlyPending = append(newlyPending, pending...)
		summary.NewPending += len(pending)
	}

	if len(newlyPending) > 0 && m.notifier != nil {
		if err := m.notifier.NotifyPendingReview(ctx, cfg, newlyPending); err != nil {
			m.logger.Warn("failed to send pending review notification", "err", err)
		}
	}

	if summary.NewApproved > 0 {
		m.TriggerDownload()
	}

	pruned, err := m.PruneAll(ctx)
	if err != nil {
		m.logger.Warn("retention prune failed", "err", err)
	}
	summary.Pruned = pruned

	return summary, nil
}

// SyncChannel syncs a single channel by ID on demand.
func (m *Manager) SyncChannel(ctx context.Context, channelID int64) (*SyncSummary, error) {
	cfg, err := m.store.GetSettings(ctx, m.defaults)
	if err != nil {
		return nil, err
	}
	ch, err := m.store.GetChannel(ctx, channelID)
	if err != nil {
		return nil, err
	}

	pending, approved, skipped, err := m.syncOneChannel(ctx, ch, cfg.CheckLimitPerChannel)
	if err != nil {
		return nil, err
	}

	if len(pending) > 0 && m.notifier != nil {
		if err := m.notifier.NotifyPendingReview(ctx, cfg, pending); err != nil {
			m.logger.Warn("failed to send pending review notification", "err", err)
		}
	}
	if approved > 0 {
		m.TriggerDownload()
	}

	return &SyncSummary{
		ChannelsChecked: 1,
		NewPending:      len(pending),
		NewApproved:     approved,
		Skipped:         skipped,
	}, nil
}

func (m *Manager) syncOneChannel(ctx context.Context, ch *db.Channel, limit int) (newlyPending []db.Video, newApproved int, skipped int, err error) {
	items, err := m.yt.ListChannelVideos(ctx, ch.URL, limit)
	if err != nil {
		return nil, 0, 0, err
	}

	now := time.Now().UTC()
	for _, item := range items {
		if ch.SkipShorts && item.IsShort {
			skipped++
			continue
		}
		if ch.SkipLivestreams && item.IsLivestream {
			skipped++
			continue
		}

		status := db.StatusPendingReview
		if ch.AutoApprove {
			status = db.StatusApproved
		}

		chID := ch.ID
		v := db.Video{
			YouTubeID:       item.YouTubeID,
			ChannelID:       &chID,
			ChannelName:     ch.Name,
			Title:           item.Title,
			Description:     item.Description,
			ThumbnailURL:    item.ThumbnailURL,
			DurationSeconds: item.DurationSeconds,
			PublishedAt:     item.PublishedAt,
			IsShort:         item.IsShort,
			IsLivestream:    item.IsLivestream,
			Status:          status,
		}

		inserted, err := m.store.InsertVideoIfNew(ctx, &v)
		if err != nil {
			m.logger.Warn("insert video failed", "youtube_id", item.YouTubeID, "err", err)
			continue
		}
		if !inserted {
			continue
		}
		if status == db.StatusApproved {
			newApproved++
		} else {
			newlyPending = append(newlyPending, v)
		}
	}

	_ = m.store.UpdateChannelLastChecked(ctx, ch.ID, now)
	return newlyPending, newApproved, skipped, nil
}

func (m *Manager) downloadLoop() {
	defer m.wg.Done()
	for {
		select {
		case <-m.stopCh:
			return
		case <-m.dlSignal:
			m.processDownloadQueue()
		}
	}
}

func (m *Manager) processDownloadQueue() {
	ctx := context.Background()
	downloadedAny := false

	for {
		select {
		case <-m.stopCh:
			return
		default:
		}

		approved, err := m.store.ListVideos(ctx, db.StatusApproved, nil, 10)
		if err != nil {
			m.logger.Error("list approved videos failed", "err", err)
			return
		}
		if len(approved) == 0 {
			break
		}

		cfg, err := m.store.GetSettings(ctx, m.defaults)
		if err != nil {
			cfg = m.defaults
		}

		pausedByHardLimit := false
		for _, v := range approved {
			select {
			case <-m.stopCh:
				return
			default:
			}

			if m.checkHardLimitBlocked(ctx, cfg) {
				pausedByHardLimit = true
				break
			}

			if err := m.store.UpdateVideoStatus(ctx, v.ID, db.StatusDownloading, ""); err != nil {
				m.logger.Error("mark downloading failed", "id", v.ID, "err", err)
				continue
			}

			m.logger.Info("downloading video", "youtube_id", v.YouTubeID, "title", v.Title, "channel", v.ChannelName)
			res, err := m.yt.DownloadVideo(ctx, v.YouTubeID, v.ChannelName, v.Title, m.mediaDir, cfg.MaxResolution)
			if err != nil {
				m.logger.Error("download failed", "youtube_id", v.YouTubeID, "err", err)
				_ = m.store.UpdateVideoStatus(ctx, v.ID, db.StatusFailed, err.Error())
				continue
			}

			if err := m.store.MarkVideoDownloaded(ctx, v.ID, res.FilePath, res.ThumbPath); err != nil {
				m.logger.Error("mark downloaded failed", "id", v.ID, "err", err)
				continue
			}
			m.logger.Info("download complete", "youtube_id", v.YouTubeID, "file", res.FilePath)
			downloadedAny = true

			// Enforce soft storage limit between downloads so a large queue won't overshoot the hard limit.
			if cfg.StorageSoftLimitGB > 0 {
				if _, err := m.pruneByStorageSoftLimit(ctx, gbToBytes(cfg.StorageSoftLimitGB)); err != nil {
					m.logger.Warn("mid-batch storage prune failed", "err", err)
				}
			}
		}
		if pausedByHardLimit {
			break
		}
	}

	if downloadedAny {
		if _, err := m.PruneAll(ctx); err != nil {
			m.logger.Warn("post-download prune failed", "err", err)
		}
		m.refreshPlex(ctx)
	}
}

// GetStorageInfo returns current disk usage in mediaDir alongside configured soft/hard limits.
func (m *Manager) GetStorageInfo(ctx context.Context) StorageInfo {
	cfg, err := m.store.GetSettings(ctx, m.defaults)
	if err != nil {
		cfg = m.defaults
	}
	used := DirSizeBytes(m.mediaDir)
	soft := gbToBytes(cfg.StorageSoftLimitGB)
	hard := gbToBytes(cfg.StorageHardLimitGB)
	return StorageInfo{
		UsedBytes:        used,
		SoftLimitBytes:   soft,
		HardLimitBytes:   hard,
		HardLimitReached: hard > 0 && used >= hard,
	}
}

// checkHardLimitBlocked checks whether downloads must pause due to the hard storage limit.
// If a soft limit is also configured, it first attempts to prune oldest videos down to the soft limit.
func (m *Manager) checkHardLimitBlocked(ctx context.Context, cfg db.Settings) bool {
	hardBytes := gbToBytes(cfg.StorageHardLimitGB)
	if hardBytes <= 0 {
		m.hardLimitNotified.Store(false)
		return false
	}

	used := DirSizeBytes(m.mediaDir)
	if used < hardBytes {
		m.hardLimitNotified.Store(false)
		return false
	}

	// Try freeing space down to soft limit if configured.
	if softBytes := gbToBytes(cfg.StorageSoftLimitGB); softBytes > 0 {
		if pruned, err := m.pruneByStorageSoftLimit(ctx, softBytes); err == nil && pruned > 0 {
			m.refreshPlex(ctx)
			used = DirSizeBytes(m.mediaDir)
			if used < hardBytes {
				m.hardLimitNotified.Store(false)
				return false
			}
		}
	}

	m.logger.Warn("pausing downloads: storage hard limit reached",
		"used_bytes", used,
		"hard_limit_bytes", hardBytes,
	)
	if m.notifier != nil && m.hardLimitNotified.CompareAndSwap(false, true) {
		if err := m.notifier.NotifyStorageHardLimit(ctx, cfg, used, hardBytes); err != nil {
			m.logger.Warn("failed to send hard storage limit notification", "err", err)
		}
	}
	return true
}

// PruneAll removes downloaded videos that exceed configured retention age, max-video limits,
// or the global soft storage limit.
func (m *Manager) PruneAll(ctx context.Context) (int, error) {
	cfg, err := m.store.GetSettings(ctx, m.defaults)
	if err != nil {
		return 0, err
	}

	channels, err := m.store.ListChannels(ctx, false)
	if err != nil {
		return 0, err
	}

	pruned := 0
	now := time.Now().UTC()

	for _, ch := range channels {
		retDays := cfg.GlobalRetentionDays
		if ch.RetentionDays != nil {
			retDays = *ch.RetentionDays
		}
		retMax := cfg.GlobalRetentionMaxVideo
		if ch.RetentionMaxVideos != nil {
			retMax = *ch.RetentionMaxVideos
		}
		if retDays <= 0 && retMax <= 0 {
			continue
		}
		chID := ch.ID
		n, err := m.pruneGroup(ctx, &chID, retDays, retMax, now)
		if err != nil {
			m.logger.Warn("prune channel failed", "channel", ch.Name, "err", err)
			continue
		}
		pruned += n
	}

	// Also apply global retention to standalone videos if configured.
	if cfg.GlobalRetentionDays > 0 || cfg.GlobalRetentionMaxVideo > 0 {
		n, err := m.pruneGroup(ctx, nil, cfg.GlobalRetentionDays, cfg.GlobalRetentionMaxVideo, now)
		if err == nil {
			pruned += n
		}
	}

	// Apply global soft storage limit (oldest downloaded videos pruned first).
	if softBytes := gbToBytes(cfg.StorageSoftLimitGB); softBytes > 0 {
		n, err := m.pruneByStorageSoftLimit(ctx, softBytes)
		if err != nil {
			m.logger.Warn("soft storage limit prune failed", "err", err)
		} else {
			pruned += n
		}
	}

	if pruned > 0 {
		m.logger.Info("pruned expired videos", "count", pruned)
		m.refreshPlex(ctx)
	}
	return pruned, nil
}

func (m *Manager) pruneByStorageSoftLimit(ctx context.Context, softLimitBytes int64) (int, error) {
	if softLimitBytes <= 0 {
		return 0, nil
	}
	used := DirSizeBytes(m.mediaDir)
	if used <= softLimitBytes {
		return 0, nil
	}

	videos, err := m.store.ListAllDownloadedOldestFirst(ctx)
	if err != nil {
		return 0, err
	}

	pruned := 0
	for _, v := range videos {
		if used <= softLimitBytes {
			break
		}
		freed := fileSize(v.FilePath) + fileSize(v.ThumbPath)
		RemoveVideoFiles(v.FilePath, v.ThumbPath)
		if err := m.store.MarkVideoPruned(ctx, v.ID); err != nil {
			m.logger.Warn("mark pruned failed", "id", v.ID, "err", err)
			continue
		}
		used -= freed
		if used < 0 {
			used = 0
		}
		pruned++
	}
	return pruned, nil
}

// DirSizeBytes returns the total size in bytes of all regular files under dir.
func DirSizeBytes(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func fileSize(path string) int64 {
	if path == "" {
		return 0
	}
	if info, err := os.Stat(path); err == nil {
		return info.Size()
	}
	return 0
}

func gbToBytes(gb float64) int64 {
	if gb <= 0 {
		return 0
	}
	return int64(gb * 1024 * 1024 * 1024)
}

func (m *Manager) pruneGroup(ctx context.Context, channelID *int64, retDays, retMax int, now time.Time) (int, error) {
	videos, err := m.store.ListDownloadedForChannel(ctx, channelID)
	if err != nil {
		return 0, err
	}

	pruned := 0
	var cutoff time.Time
	if retDays > 0 {
		cutoff = now.Add(-time.Duration(retDays) * 24 * time.Hour)
	}

	for idx, v := range videos {
		shouldPrune := false
		if retMax > 0 && idx >= retMax {
			shouldPrune = true
		}
		if retDays > 0 {
			refTime := v.CreatedAt
			if v.DownloadedAt != nil {
				refTime = *v.DownloadedAt
			}
			if refTime.Before(cutoff) {
				shouldPrune = true
			}
		}
		if !shouldPrune {
			continue
		}

		RemoveVideoFiles(v.FilePath, v.ThumbPath)
		if err := m.store.MarkVideoPruned(ctx, v.ID); err != nil {
			m.logger.Warn("mark pruned failed", "id", v.ID, "err", err)
			continue
		}
		pruned++
	}
	return pruned, nil
}

// RemoveVideoFiles deletes the downloaded video and its thumbnail from disk if present.
func RemoveVideoFiles(filePath, thumbPath string) {
	if filePath != "" {
		_ = os.Remove(filePath)
	}
	if thumbPath != "" {
		_ = os.Remove(thumbPath)
	}
}

func (m *Manager) refreshPlex(ctx context.Context) {
	if m.plex == nil {
		return
	}
	cfg, err := m.store.GetSettings(ctx, m.defaults)
	if err != nil || cfg.PlexURL == "" || cfg.PlexToken == "" {
		return
	}
	if err := m.plex.RefreshLibrary(ctx, cfg.PlexURL, cfg.PlexToken, cfg.PlexSectionID); err != nil {
		m.logger.Warn("plex library refresh failed", "err", err)
	} else {
		m.logger.Info("triggered plex library refresh", "section", cfg.PlexSectionID)
	}
}
