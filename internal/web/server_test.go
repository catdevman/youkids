package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/catdevman/youkids/internal/db"
	"github.com/catdevman/youkids/internal/notify"
	"github.com/catdevman/youkids/internal/plex"
	"github.com/catdevman/youkids/internal/worker"
	"github.com/catdevman/youkids/internal/ytdlp"
)

type fakeYT struct{}

func (f *fakeYT) ResolveChannel(ctx context.Context, input string) (*ytdlp.ChannelInfo, error) {
	return &ytdlp.ChannelInfo{YouTubeID: "UC_demo", Name: "Demo Kids", URL: "https://www.youtube.com/@demokids"}, nil
}
func (f *fakeYT) ListChannelVideos(ctx context.Context, channelURL string, limit int) ([]ytdlp.VideoInfo, error) {
	return []ytdlp.VideoInfo{{YouTubeID: "v123", Title: "Fun Science", DurationSeconds: 320}}, nil
}
func (f *fakeYT) ResolveVideo(ctx context.Context, input string) (*ytdlp.VideoInfo, error) {
	return &ytdlp.VideoInfo{YouTubeID: "single99", Title: "Single Video", ChannelName: "Standalone", DurationSeconds: 200}, nil
}
func (f *fakeYT) DownloadVideo(ctx context.Context, youtubeID, channelName, title, mediaDir string, maxRes int) (*ytdlp.DownloadResult, error) {
	return &ytdlp.DownloadResult{FilePath: filepath.Join(mediaDir, youtubeID+".mp4")}, nil
}

func TestAPIFlowAndAuth(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()

	defaults := db.DefaultSettings("0 0 * * *", "", "", "", "")
	yt := &fakeYT{}
	mgr := worker.NewManager(store, defaults, t.TempDir(), yt, plex.NewClient(), notify.NewNotifier(), nil)
	srv := NewServer(store, defaults, t.TempDir(), "parentpass", yt, plex.NewClient(), notify.NewNotifier(), mgr, nil)
	handler := srv.Handler()

	// 1. Healthz bypasses auth
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz expected 200, got %d", rec.Code)
	}

	// 2. API requires password when configured
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d", rec.Code)
	}

	// 3. Add channel with sync_now=true
	body := bytes.NewBufferString(`{"input":"@demokids","auto_approve":false,"sync_now":true}`)
	req = httptest.NewRequest(http.MethodPost, "/api/channels", body)
	req.SetBasicAuth("parent", "parentpass")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create channel expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// 4. Verify pending video appears in review queue
	req = httptest.NewRequest(http.MethodGet, "/api/videos?status=pending_review", nil)
	req.SetBasicAuth("parent", "parentpass")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list videos expected 200, got %d", rec.Code)
	}
	var vids []db.Video
	if err := json.Unmarshal(rec.Body.Bytes(), &vids); err != nil || len(vids) != 1 {
		t.Fatalf("expected 1 pending video, got %d (err=%v)", len(vids), err)
	}

	// 5. Reject invalid cron schedule in settings
	badSettings := bytes.NewBufferString(`{"sync_cron":"not-a-cron"}`)
	req = httptest.NewRequest(http.MethodPut, "/api/settings", badSettings)
	req.SetBasicAuth("parent", "parentpass")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad cron, got %d", rec.Code)
	}

	// 6. Reject soft storage limit > hard storage limit
	badLimits := bytes.NewBufferString(`{"sync_cron":"0 0 * * *","storage_soft_limit_gb":100,"storage_hard_limit_gb":50}`)
	req = httptest.NewRequest(http.MethodPut, "/api/settings", badLimits)
	req.SetBasicAuth("parent", "parentpass")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when soft > hard limit, got %d", rec.Code)
	}
}
