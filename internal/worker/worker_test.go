package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/catdevman/youkids/internal/db"
	"github.com/catdevman/youkids/internal/notify"
	"github.com/catdevman/youkids/internal/plex"
	"github.com/catdevman/youkids/internal/ytdlp"
)

type mockYT struct {
	videosByURL map[string][]ytdlp.VideoInfo
}

func (m *mockYT) ResolveChannel(ctx context.Context, input string) (*ytdlp.ChannelInfo, error) {
	return &ytdlp.ChannelInfo{YouTubeID: "UCtest", Name: "Test Channel", URL: "https://www.youtube.com/@test"}, nil
}

func (m *mockYT) ListChannelVideos(ctx context.Context, channelURL string, limit int) ([]ytdlp.VideoInfo, error) {
	return m.videosByURL[channelURL], nil
}

func (m *mockYT) ResolveVideo(ctx context.Context, input string) (*ytdlp.VideoInfo, error) {
	return &ytdlp.VideoInfo{YouTubeID: input, Title: "Standalone Video", ChannelName: "Standalone", DurationSeconds: 300}, nil
}

func (m *mockYT) DownloadVideo(ctx context.Context, youtubeID, channelName, title, mediaDir string, maxRes int) (*ytdlp.DownloadResult, error) {
	dir := filepath.Join(mediaDir, ytdlp.SanitizeFilename(channelName))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	base := ytdlp.SanitizeFilename(title) + " [" + youtubeID + "]"
	mp4 := filepath.Join(dir, base+".mp4")
	jpg := filepath.Join(dir, base+".jpg")
	if err := os.WriteFile(mp4, []byte("fake mp4"), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(jpg, []byte("fake jpg"), 0o644); err != nil {
		return nil, err
	}
	return &ytdlp.DownloadResult{FilePath: mp4, ThumbPath: jpg}, nil
}

func TestSyncApproveFilterNotifyAndPrune(t *testing.T) {
	ctx := context.Background()
	mediaDir := t.TempDir()

	store, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()

	var plexRefreshes atomic.Int32
	plexSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "secret-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		plexRefreshes.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer plexSrv.Close()

	var notifiedCount atomic.Int32
	webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Count int `json:"count"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		notifiedCount.Add(int32(payload.Count))
		w.WriteHeader(http.StatusOK)
	}))
	defer webhookSrv.Close()

	defaults := db.DefaultSettings("0 0 * * *", "http://youkids.local", plexSrv.URL, "secret-token", "4")
	defaults.NotifyGenericWebhookURL = webhookSrv.URL
	if err := store.SaveSettings(ctx, defaults); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	// Channel 1: requires review, skips shorts & livestreams
	chReview := &db.Channel{
		YouTubeID:       "UC_review",
		Name:            "Review Channel",
		URL:             "https://www.youtube.com/@review",
		AutoApprove:     false,
		Enabled:         true,
		SkipShorts:      true,
		SkipLivestreams: true,
	}
	if err := store.CreateChannel(ctx, chReview); err != nil {
		t.Fatalf("create chReview: %v", err)
	}

	// Channel 2: auto-approve with max 1 video retention
	maxOne := 1
	chAuto := &db.Channel{
		YouTubeID:          "UC_auto",
		Name:               "Auto Channel",
		URL:                "https://www.youtube.com/@auto",
		AutoApprove:        true,
		Enabled:            true,
		SkipShorts:         true,
		SkipLivestreams:    true,
		RetentionMaxVideos: &maxOne,
	}
	if err := store.CreateChannel(ctx, chAuto); err != nil {
		t.Fatalf("create chAuto: %v", err)
	}

	yt := &mockYT{
		videosByURL: map[string][]ytdlp.VideoInfo{
			chReview.URL: {
				{YouTubeID: "vid_ok_1", Title: "Good Episode", DurationSeconds: 600},
				{YouTubeID: "vid_short", Title: "A Short", DurationSeconds: 30, IsShort: true},
				{YouTubeID: "vid_live", Title: "Live Stream", DurationSeconds: 1200, IsLivestream: true},
			},
			chAuto.URL: {
				{YouTubeID: "auto_1", Title: "Auto Ep 1", DurationSeconds: 500},
				{YouTubeID: "auto_2", Title: "Auto Ep 2", DurationSeconds: 500},
			},
		},
	}

	mgr := NewManager(store, defaults, mediaDir, yt, plex.NewClient(), notify.NewNotifier(), nil)
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	defer mgr.Stop()

	summary, err := mgr.SyncAll(ctx)
	if err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if summary.ChannelsChecked != 2 || summary.NewPending != 1 || summary.NewApproved != 2 || summary.Skipped != 2 {
		t.Fatalf("unexpected sync summary: %+v", summary)
	}
	if notifiedCount.Load() != 1 {
		t.Fatalf("expected 1 video notified, got %d", notifiedCount.Load())
	}

	// Wait for background download worker to finish downloading both auto-approved videos and pruning to 1.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := store.GetStats(ctx)
		if st.Approved == 0 && st.Downloading == 0 && st.Downloaded == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	st, err := store.GetStats(ctx)
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if st.Downloaded != 1 {
		t.Fatalf("expected 1 downloaded video after max=1 retention prune, got stats %+v", st)
	}
	if plexRefreshes.Load() < 1 {
		t.Fatalf("expected at least 1 Plex library refresh, got %d", plexRefreshes.Load())
	}

	// Second sync should not re-add pruned or already-seen videos.
	summary2, err := mgr.SyncAll(ctx)
	if err != nil {
		t.Fatalf("second SyncAll: %v", err)
	}
	if summary2.NewPending != 0 || summary2.NewApproved != 0 {
		t.Fatalf("expected 0 new videos on second sync, got %+v", summary2)
	}
}

func TestStorageSoftAndHardLimits(t *testing.T) {
	ctx := context.Background()
	mediaDir := t.TempDir()

	store, err := db.Open(filepath.Join(t.TempDir(), "storage.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()

	// Create 3 fake downloaded videos (1000 bytes each = 3000 bytes total)
	chDir := filepath.Join(mediaDir, "Kids")
	if err := os.MkdirAll(chDir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 1000)
	for i, ytID := range []string{"old_1", "old_2", "new_3"} {
		fp := filepath.Join(chDir, ytID+".mp4")
		if err := os.WriteFile(fp, payload, 0o644); err != nil {
			t.Fatal(err)
		}
		dlAt := time.Now().UTC().Add(time.Duration(i-5) * time.Hour)
		v := &db.Video{
			YouTubeID:    ytID,
			ChannelName:  "Kids",
			Title:        ytID,
			Status:       db.StatusDownloaded,
			FilePath:     fp,
			DownloadedAt: &dlAt,
		}
		if _, err := store.InsertVideoIfNew(ctx, v); err != nil {
			t.Fatal(err)
		}
	}

	// Set soft limit to ~2500 bytes (should prune old_1, leaving old_2 + new_3 = 2000 bytes)
	softGB := 2500.0 / (1024 * 1024 * 1024)
	defaults := db.DefaultSettings("0 0 * * *", "", "", "", "")
	defaults.StorageSoftLimitGB = softGB
	if err := store.SaveSettings(ctx, defaults); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(store, defaults, mediaDir, &mockYT{}, nil, nil, nil)
	pruned, err := mgr.PruneAll(ctx)
	if err != nil {
		t.Fatalf("PruneAll: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("expected 1 pruned video under soft limit, got %d", pruned)
	}
	v1, _ := store.GetVideoByYouTubeID(ctx, "old_1")
	if v1.Status != db.StatusPruned {
		t.Fatalf("expected old_1 to be pruned, got %s", v1.Status)
	}
	if got := DirSizeBytes(mediaDir); got != 2000 {
		t.Fatalf("expected 2000 bytes remaining, got %d", got)
	}

	// Now set hard limit to 1500 bytes with soft limit disabled (0) so downloads must pause.
	defaults.StorageSoftLimitGB = 0
	defaults.StorageHardLimitGB = 1500.0 / (1024 * 1024 * 1024)
	if err := store.SaveSettings(ctx, defaults); err != nil {
		t.Fatal(err)
	}

	queued := &db.Video{
		YouTubeID:   "queued_1",
		ChannelName: "Kids",
		Title:       "Queued Ep",
		Status:      db.StatusApproved,
	}
	if _, err := store.InsertVideoIfNew(ctx, queued); err != nil {
		t.Fatal(err)
	}

	mgr.processDownloadQueue()

	afterBlocked, _ := store.GetVideoByYouTubeID(ctx, "queued_1")
	if afterBlocked.Status != db.StatusApproved {
		t.Fatalf("expected queued_1 to remain approved when hard limit reached, got %s", afterBlocked.Status)
	}
	if info := mgr.GetStorageInfo(ctx); !info.HardLimitReached {
		t.Fatalf("expected HardLimitReached=true, got %+v", info)
	}

	// Raise hard limit to 1 GB and re-run download queue -> queued_1 should download!
	defaults.StorageHardLimitGB = 1.0
	if err := store.SaveSettings(ctx, defaults); err != nil {
		t.Fatal(err)
	}
	mgr.processDownloadQueue()

	afterResumed, _ := store.GetVideoByYouTubeID(ctx, "queued_1")
	if afterResumed.Status != db.StatusDownloaded {
		t.Fatalf("expected queued_1 to download after raising hard limit, got %s", afterResumed.Status)
	}
}
