package ytdlp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ChannelInfo contains metadata resolved from a YouTube channel.
type ChannelInfo struct {
	YouTubeID string
	Name      string
	URL       string
	AvatarURL string
}

// VideoInfo contains metadata for a discovered or standalone YouTube video.
type VideoInfo struct {
	YouTubeID       string
	ChannelID       string
	ChannelName     string
	Title           string
	Description     string
	ThumbnailURL    string
	DurationSeconds int
	PublishedAt     *time.Time
	IsShort         bool
	IsLivestream    bool
}

// DownloadResult holds the local paths produced by downloading a video.
type DownloadResult struct {
	FilePath  string
	ThumbPath string
}

// Client abstracts yt-dlp interactions for easy testing.
type Client interface {
	ResolveChannel(ctx context.Context, input string) (*ChannelInfo, error)
	ListChannelVideos(ctx context.Context, channelURL string, limit int) ([]VideoInfo, error)
	ResolveVideo(ctx context.Context, input string) (*VideoInfo, error)
	DownloadVideo(ctx context.Context, youtubeID, channelName, title, mediaDir string, maxRes int) (*DownloadResult, error)
}

// Runner implements Client by invoking the yt-dlp binary.
type Runner struct {
	Bin         string
	CookiesFile string
}

// NewRunner creates a yt-dlp CLI runner.
func NewRunner(bin, cookiesFile string) *Runner {
	if bin == "" {
		bin = "yt-dlp"
	}
	return &Runner{
		Bin:         bin,
		CookiesFile: cookiesFile,
	}
}

func (r *Runner) baseArgs() []string {
	args := []string{"--no-warnings", "--ignore-errors"}
	if r.CookiesFile != "" {
		args = append(args, "--cookies", r.CookiesFile)
	}
	return args
}

// NormalizeChannelURL turns a channel ID (UC...), @handle, or URL into a canonical YouTube channel URL.
func NormalizeChannelURL(input string) string {
	s := strings.TrimSpace(input)
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return strings.TrimRight(s, "/")
	}
	if strings.HasPrefix(s, "@") {
		return "https://www.youtube.com/" + s
	}
	if strings.HasPrefix(s, "UC") && len(s) >= 20 && !strings.Contains(s, "/") {
		return "https://www.youtube.com/channel/" + s
	}
	return "https://www.youtube.com/@" + strings.TrimPrefix(s, "@")
}

// ChannelVideosURL ensures a channel URL targets its /videos tab.
func ChannelVideosURL(channelURL string) string {
	u := strings.TrimRight(channelURL, "/")
	for _, suffix := range []string{"/videos", "/shorts", "/streams", "/playlists", "/featured"} {
		if strings.HasSuffix(u, suffix) {
			return u
		}
	}
	return u + "/videos"
}

// NormalizeVideoURL turns a video ID or URL into a canonical YouTube watch URL.
func NormalizeVideoURL(input string) string {
	s := strings.TrimSpace(input)
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s
	}
	return "https://www.youtube.com/watch?v=" + s
}

func (r *Runner) ResolveChannel(ctx context.Context, input string) (*ChannelInfo, error) {
	chURL := NormalizeChannelURL(input)
	videosURL := ChannelVideosURL(chURL)

	args := append(r.baseArgs(),
		"--dump-single-json",
		"--flat-playlist",
		"--playlist-end", "1",
		videosURL,
	)

	out, err := r.exec(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("resolve channel %q: %w", input, err)
	}

	var meta struct {
		ID          string `json:"id"`
		ChannelID   string `json:"channel_id"`
		Channel     string `json:"channel"`
		Uploader    string `json:"uploader"`
		Title       string `json:"title"`
		WebpageURL  string `json:"webpage_url"`
		ChannelURL  string `json:"channel_url"`
		Thumbnails  []struct {
			URL string `json:"url"`
		} `json:"thumbnails"`
	}
	if err := json.Unmarshal(out, &meta); err != nil {
		return nil, fmt.Errorf("parse channel metadata: %w", err)
	}

	ytID := meta.ChannelID
	if ytID == "" {
		ytID = meta.ID
	}
	if ytID == "" {
		return nil, fmt.Errorf("could not determine channel ID for %q", input)
	}

	name := meta.Channel
	if name == "" {
		name = meta.Uploader
	}
	if name == "" {
		name = strings.TrimSuffix(meta.Title, " - Videos")
	}
	if name == "" {
		name = ytID
	}

	canonicalURL := meta.ChannelURL
	if canonicalURL == "" {
		canonicalURL = strings.TrimSuffix(meta.WebpageURL, "/videos")
	}
	if canonicalURL == "" {
		canonicalURL = chURL
	}

	var avatar string
	if len(meta.Thumbnails) > 0 {
		avatar = meta.Thumbnails[len(meta.Thumbnails)-1].URL
	}

	return &ChannelInfo{
		YouTubeID: ytID,
		Name:      name,
		URL:       strings.TrimRight(canonicalURL, "/"),
		AvatarURL: avatar,
	}, nil
}

func (r *Runner) ListChannelVideos(ctx context.Context, channelURL string, limit int) ([]VideoInfo, error) {
	if limit <= 0 {
		limit = 15
	}
	videosURL := ChannelVideosURL(channelURL)
	args := append(r.baseArgs(),
		"--flat-playlist",
		"--dump-json",
		"--playlist-end", fmt.Sprintf("%d", limit),
		videosURL,
	)

	out, err := r.exec(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("list channel videos %q: %w", channelURL, err)
	}

	var videos []VideoInfo
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		v, ok := parseVideoJSON([]byte(line))
		if ok {
			videos = append(videos, v)
		}
	}
	return videos, sc.Err()
}

func (r *Runner) ResolveVideo(ctx context.Context, input string) (*VideoInfo, error) {
	videoURL := NormalizeVideoURL(input)
	args := append(r.baseArgs(),
		"--dump-single-json",
		"--no-playlist",
		videoURL,
	)
	out, err := r.exec(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("resolve video %q: %w", input, err)
	}
	v, ok := parseVideoJSON(out)
	if !ok {
		return nil, fmt.Errorf("failed to parse video metadata for %q", input)
	}
	return &v, nil
}

// DownloadVideo downloads a video into Plex "Other Videos" layout:
// <mediaDir>/<Sanitized Channel Name>/<Sanitized Title> [<youtube_id>].mp4
// along with a matching .jpg thumbnail.
func (r *Runner) DownloadVideo(ctx context.Context, youtubeID, channelName, title, mediaDir string, maxRes int) (*DownloadResult, error) {
	if maxRes <= 0 {
		maxRes = 1080
	}
	if channelName == "" {
		channelName = "Standalone"
	}

	dirName := SanitizeFilename(channelName)
	baseName := fmt.Sprintf("%s [%s]", SanitizeFilename(title), youtubeID)
	channelDir := filepath.Join(mediaDir, dirName)
	if err := os.MkdirAll(channelDir, 0o755); err != nil {
		return nil, fmt.Errorf("create channel dir: %w", err)
	}

	outputTemplate := filepath.Join(channelDir, baseName+".%(ext)s")
	formatSelector := fmt.Sprintf(
		"bestvideo[height<=%d][ext=mp4]+bestaudio[ext=m4a]/best[height<=%d][ext=mp4]/bestvideo[height<=%d]+bestaudio/best[height<=%d]/best",
		maxRes, maxRes, maxRes, maxRes,
	)

	args := append(r.baseArgs(),
		"--no-playlist",
		"-f", formatSelector,
		"--merge-output-format", "mp4",
		"--write-thumbnail",
		"--convert-thumbnails", "jpg",
		"--embed-metadata",
		"-o", outputTemplate,
		NormalizeVideoURL(youtubeID),
	)

	if _, err := r.exec(ctx, args...); err != nil {
		return nil, fmt.Errorf("download video %s: %w", youtubeID, err)
	}

	videoPath := filepath.Join(channelDir, baseName+".mp4")
	if _, err := os.Stat(videoPath); err != nil {
		// Fallback: locate any downloaded video file with the matching prefix.
		matches, _ := filepath.Glob(filepath.Join(channelDir, baseName+".*"))
		for _, m := range matches {
			ext := strings.ToLower(filepath.Ext(m))
			if ext == ".mp4" || ext == ".mkv" || ext == ".webm" {
				videoPath = m
				break
			}
		}
		if _, err := os.Stat(videoPath); err != nil {
			return nil, fmt.Errorf("downloaded file not found at %s", videoPath)
		}
	}

	thumbPath := filepath.Join(channelDir, baseName+".jpg")
	if _, err := os.Stat(thumbPath); err != nil {
		thumbPath = ""
	}

	return &DownloadResult{
		FilePath:  videoPath,
		ThumbPath: thumbPath,
	}, nil
}

func (r *Runner) exec(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.Bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return stdout.Bytes(), nil
}

type rawVideoJSON struct {
	ID             string  `json:"id"`
	Title          string  `json:"title"`
	Description    string  `json:"description"`
	ChannelID      string  `json:"channel_id"`
	Channel        string  `json:"channel"`
	Uploader       string  `json:"uploader"`
	URL            string  `json:"url"`
	WebpageURL     string  `json:"webpage_url"`
	OriginalURL    string  `json:"original_url"`
	Thumbnail      string  `json:"thumbnail"`
	Duration       float64 `json:"duration"`
	UploadDate     string  `json:"upload_date"`
	Timestamp      int64   `json:"timestamp"`
	ReleaseTS      int64   `json:"release_timestamp"`
	LiveStatus     string  `json:"live_status"`
	IsLive         bool    `json:"is_live"`
	WasLive        bool    `json:"was_live"`
	Thumbnails     []struct {
		URL string `json:"url"`
	} `json:"thumbnails"`
}

func parseVideoJSON(data []byte) (VideoInfo, bool) {
	var raw rawVideoJSON
	if err := json.Unmarshal(data, &raw); err != nil || raw.ID == "" {
		return VideoInfo{}, false
	}

	thumb := raw.Thumbnail
	if thumb == "" && len(raw.Thumbnails) > 0 {
		thumb = raw.Thumbnails[len(raw.Thumbnails)-1].URL
	}
	if thumb == "" {
		thumb = fmt.Sprintf("https://i.ytimg.com/vi/%s/hqdefault.jpg", raw.ID)
	}

	chName := raw.Channel
	if chName == "" {
		chName = raw.Uploader
	}

	var pub *time.Time
	if raw.Timestamp > 0 {
		t := time.Unix(raw.Timestamp, 0).UTC()
		pub = &t
	} else if raw.ReleaseTS > 0 {
		t := time.Unix(raw.ReleaseTS, 0).UTC()
		pub = &t
	} else if len(raw.UploadDate) == 8 {
		if t, err := time.Parse("20060102", raw.UploadDate); err == nil {
			u := t.UTC()
			pub = &u
		}
	}

	urlCombined := strings.ToLower(raw.URL + " " + raw.WebpageURL + " " + raw.OriginalURL)
	isShort := strings.Contains(urlCombined, "/shorts/") || (raw.Duration > 0 && raw.Duration <= 60)

	liveStatus := strings.ToLower(raw.LiveStatus)
	isLive := raw.IsLive || raw.WasLive ||
		(liveStatus != "" && liveStatus != "not_live")

	return VideoInfo{
		YouTubeID:       raw.ID,
		ChannelID:       raw.ChannelID,
		ChannelName:     chName,
		Title:           raw.Title,
		Description:     raw.Description,
		ThumbnailURL:    thumb,
		DurationSeconds: int(raw.Duration),
		PublishedAt:     pub,
		IsShort:         isShort,
		IsLivestream:    isLive,
	}, true
}

var unsafeChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)
var multiSpaces = regexp.MustCompile(`\s+`)

// SanitizeFilename produces a clean, Plex-friendly, cross-platform filename or folder name.
func SanitizeFilename(name string) string {
	s := unsafeChars.ReplaceAllString(name, " ")
	s = multiSpaces.ReplaceAllString(s, " ")
	s = strings.Trim(s, " .")
	if s == "" {
		return "Untitled"
	}
	runes := []rune(s)
	if len(runes) > 120 {
		s = strings.TrimSpace(string(runes[:120]))
	}
	return s
}
