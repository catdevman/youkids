package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

// Store wraps SQLite persistence for YouKids.
type Store struct {
	db *sql.DB
}

// Open opens or creates the SQLite database at dbPath and runs schema migrations.
func Open(dbPath string) (*Store, error) {
	if dbPath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
			return nil, fmt.Errorf("create db directory: %w", err)
		}
	}

	dsn := dbPath
	if dbPath != ":memory:" {
		dsn = fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)", dbPath)
	} else {
		dsn = "file::memory:?cache=shared&_pragma=foreign_keys(ON)"
	}

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite works best with a single writer connection in-process to avoid SQLITE_BUSY.
	sqlDB.SetMaxOpenConns(1)

	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	s := &Store{db: sqlDB}
	if err := s.migrate(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate sqlite: %w", err)
	}
	return s, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS channels (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		youtube_id TEXT NOT NULL UNIQUE,
		name TEXT NOT NULL,
		url TEXT NOT NULL,
		avatar_url TEXT NOT NULL DEFAULT '',
		auto_approve INTEGER NOT NULL DEFAULT 0,
		enabled INTEGER NOT NULL DEFAULT 1,
		skip_shorts INTEGER NOT NULL DEFAULT 1,
		skip_livestreams INTEGER NOT NULL DEFAULT 1,
		retention_days INTEGER,
		retention_max_videos INTEGER,
		last_checked_at DATETIME,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS videos (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		youtube_id TEXT NOT NULL UNIQUE,
		channel_id INTEGER REFERENCES channels(id) ON DELETE SET NULL,
		channel_name TEXT NOT NULL DEFAULT 'Standalone',
		title TEXT NOT NULL,
		description TEXT NOT NULL DEFAULT '',
		thumbnail_url TEXT NOT NULL DEFAULT '',
		duration_seconds INTEGER NOT NULL DEFAULT 0,
		published_at DATETIME,
		is_short INTEGER NOT NULL DEFAULT 0,
		is_livestream INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'pending_review',
		file_path TEXT NOT NULL DEFAULT '',
		thumb_path TEXT NOT NULL DEFAULT '',
		error_message TEXT NOT NULL DEFAULT '',
		downloaded_at DATETIME,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_videos_status ON videos(status);
	CREATE INDEX IF NOT EXISTS idx_videos_channel_id ON videos(channel_id);

	CREATE TABLE IF NOT EXISTS settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	`
	_, err := s.db.Exec(schema)
	return err
}

// DefaultSettings returns baseline settings seeded with optional startup values.
func DefaultSettings(syncCron, baseURL, plexURL, plexToken, plexSectionID string) Settings {
	if syncCron == "" {
		syncCron = "0 0 * * *"
	}
	return Settings{
		SyncCron:                syncCron,
		CheckLimitPerChannel:    15,
		MaxResolution:           1080,
		DefaultSkipShorts:       true,
		DefaultSkipLivestreams:  true,
		GlobalRetentionDays:     0,
		GlobalRetentionMaxVideo: 0,
		BaseURL:                 baseURL,
		PlexURL:                 plexURL,
		PlexToken:               plexToken,
		PlexSectionID:           plexSectionID,
	}
}

// GetSettings loads stored settings, merging any missing fields with defaults.
func (s *Store) GetSettings(ctx context.Context, defaults Settings) (Settings, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'app_settings'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return defaults, nil
	}
	if err != nil {
		return defaults, err
	}
	out := defaults
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return defaults, fmt.Errorf("unmarshal settings: %w", err)
	}
	if out.SyncCron == "" {
		out.SyncCron = defaults.SyncCron
	}
	if out.CheckLimitPerChannel <= 0 {
		out.CheckLimitPerChannel = 15
	}
	if out.MaxResolution <= 0 {
		out.MaxResolution = 1080
	}
	return out, nil
}

// SaveSettings persists the application settings JSON.
func (s *Store) SaveSettings(ctx context.Context, cfg Settings) error {
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO settings(key, value) VALUES('app_settings', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		string(b),
	)
	return err
}

// --- Channels ---

func (s *Store) CreateChannel(ctx context.Context, ch *Channel) error {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO channels (
			youtube_id, name, url, avatar_url, auto_approve, enabled,
			skip_shorts, skip_livestreams, retention_days, retention_max_videos
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ch.YouTubeID, ch.Name, ch.URL, ch.AvatarURL,
		boolToInt(ch.AutoApprove), boolToInt(ch.Enabled),
		boolToInt(ch.SkipShorts), boolToInt(ch.SkipLivestreams),
		ch.RetentionDays, ch.RetentionMaxVideos,
	)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	ch.ID = id
	ch.CreatedAt = time.Now().UTC()
	return nil
}

func (s *Store) UpdateChannel(ctx context.Context, ch *Channel) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE channels SET
			name = ?,
			url = ?,
			avatar_url = ?,
			auto_approve = ?,
			enabled = ?,
			skip_shorts = ?,
			skip_livestreams = ?,
			retention_days = ?,
			retention_max_videos = ?
		WHERE id = ?`,
		ch.Name, ch.URL, ch.AvatarURL,
		boolToInt(ch.AutoApprove), boolToInt(ch.Enabled),
		boolToInt(ch.SkipShorts), boolToInt(ch.SkipLivestreams),
		ch.RetentionDays, ch.RetentionMaxVideos, ch.ID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) UpdateChannelLastChecked(ctx context.Context, id int64, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE channels SET last_checked_at = ? WHERE id = ?`, t.UTC(), id)
	return err
}

func (s *Store) DeleteChannel(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM channels WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetChannel(ctx context.Context, id int64) (*Channel, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, youtube_id, name, url, avatar_url, auto_approve, enabled,
		       skip_shorts, skip_livestreams, retention_days, retention_max_videos,
		       last_checked_at, created_at
		FROM channels WHERE id = ?`, id)
	ch, err := scanChannel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return ch, err
}

func (s *Store) ListChannels(ctx context.Context, onlyEnabled bool) ([]Channel, error) {
	query := `
		SELECT c.id, c.youtube_id, c.name, c.url, c.avatar_url, c.auto_approve, c.enabled,
		       c.skip_shorts, c.skip_livestreams, c.retention_days, c.retention_max_videos,
		       c.last_checked_at, c.created_at,
		       COALESCE(SUM(CASE WHEN v.status = 'pending_review' THEN 1 ELSE 0 END), 0) AS pending_count,
		       COALESCE(SUM(CASE WHEN v.status = 'downloaded' THEN 1 ELSE 0 END), 0) AS downloaded_count
		FROM channels c
		LEFT JOIN videos v ON v.channel_id = c.id`
	if onlyEnabled {
		query += ` WHERE c.enabled = 1`
	}
	query += ` GROUP BY c.id ORDER BY c.name COLLATE NOCASE ASC`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Channel
	for rows.Next() {
		var ch Channel
		var autoApprove, enabled, skipShorts, skipLive int
		var retDays, retMax sql.NullInt64
		var lastChecked sql.NullTime
		if err := rows.Scan(
			&ch.ID, &ch.YouTubeID, &ch.Name, &ch.URL, &ch.AvatarURL,
			&autoApprove, &enabled, &skipShorts, &skipLive,
			&retDays, &retMax, &lastChecked, &ch.CreatedAt,
			&ch.PendingCount, &ch.DownloadedCount,
		); err != nil {
			return nil, err
		}
		ch.AutoApprove = autoApprove != 0
		ch.Enabled = enabled != 0
		ch.SkipShorts = skipShorts != 0
		ch.SkipLivestreams = skipLive != 0
		if retDays.Valid {
			v := int(retDays.Int64)
			ch.RetentionDays = &v
		}
		if retMax.Valid {
			v := int(retMax.Int64)
			ch.RetentionMaxVideos = &v
		}
		if lastChecked.Valid {
			t := lastChecked.Time
			ch.LastCheckedAt = &t
		}
		out = append(out, ch)
	}
	return out, rows.Err()
}

// --- Videos ---

// InsertVideoIfNew inserts a video if its youtube_id does not already exist.
// Returns (true, id, nil) if inserted, or (false, existingID, nil) if already present.
func (s *Store) InsertVideoIfNew(ctx context.Context, v *Video) (bool, error) {
	var existingID int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM videos WHERE youtube_id = ?`, v.YouTubeID).Scan(&existingID)
	if err == nil {
		v.ID = existingID
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO videos (
			youtube_id, channel_id, channel_name, title, description, thumbnail_url,
			duration_seconds, published_at, is_short, is_livestream, status,
			file_path, thumb_path, error_message, downloaded_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.YouTubeID, v.ChannelID, v.ChannelName, v.Title, v.Description, v.ThumbnailURL,
		v.DurationSeconds, v.PublishedAt, boolToInt(v.IsShort), boolToInt(v.IsLivestream),
		string(v.Status), v.FilePath, v.ThumbPath, v.ErrorMessage, v.DownloadedAt,
	)
	if err != nil {
		return false, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return false, err
	}
	v.ID = id
	v.CreatedAt = time.Now().UTC()
	return true, nil
}

func (s *Store) GetVideo(ctx context.Context, id int64) (*Video, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, youtube_id, channel_id, channel_name, title, description, thumbnail_url,
		       duration_seconds, published_at, is_short, is_livestream, status,
		       file_path, thumb_path, error_message, downloaded_at, created_at
		FROM videos WHERE id = ?`, id)
	v, err := scanVideo(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

func (s *Store) GetVideoByYouTubeID(ctx context.Context, ytID string) (*Video, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, youtube_id, channel_id, channel_name, title, description, thumbnail_url,
		       duration_seconds, published_at, is_short, is_livestream, status,
		       file_path, thumb_path, error_message, downloaded_at, created_at
		FROM videos WHERE youtube_id = ?`, ytID)
	v, err := scanVideo(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

func (s *Store) ListVideos(ctx context.Context, status VideoStatus, channelID *int64, limit int) ([]Video, error) {
	query := `
		SELECT id, youtube_id, channel_id, channel_name, title, description, thumbnail_url,
		       duration_seconds, published_at, is_short, is_livestream, status,
		       file_path, thumb_path, error_message, downloaded_at, created_at
		FROM videos WHERE 1=1`
	var args []any
	if status != "" {
		query += ` AND status = ?`
		args = append(args, string(status))
	}
	if channelID != nil {
		query += ` AND channel_id = ?`
		args = append(args, *channelID)
	}
	query += ` ORDER BY COALESCE(published_at, created_at) DESC, id DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// ListDownloadedForChannel returns downloaded videos for a channel (or standalone if channelID is nil),
// ordered newest first so retention pruning can easily keep the first N and prune the rest.
func (s *Store) ListDownloadedForChannel(ctx context.Context, channelID *int64) ([]Video, error) {
	var query string
	var args []any
	if channelID == nil {
		query = `
			SELECT id, youtube_id, channel_id, channel_name, title, description, thumbnail_url,
			       duration_seconds, published_at, is_short, is_livestream, status,
			       file_path, thumb_path, error_message, downloaded_at, created_at
			FROM videos WHERE status = 'downloaded' AND channel_id IS NULL
			ORDER BY COALESCE(downloaded_at, created_at) DESC, id DESC`
	} else {
		query = `
			SELECT id, youtube_id, channel_id, channel_name, title, description, thumbnail_url,
			       duration_seconds, published_at, is_short, is_livestream, status,
			       file_path, thumb_path, error_message, downloaded_at, created_at
			FROM videos WHERE status = 'downloaded' AND channel_id = ?
			ORDER BY COALESCE(downloaded_at, created_at) DESC, id DESC`
		args = append(args, *channelID)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// ListAllDownloadedOldestFirst returns all downloaded videos ordered oldest first
// so storage-limit pruning removes the oldest videos first.
func (s *Store) ListAllDownloadedOldestFirst(ctx context.Context) ([]Video, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, youtube_id, channel_id, channel_name, title, description, thumbnail_url,
		       duration_seconds, published_at, is_short, is_livestream, status,
		       file_path, thumb_path, error_message, downloaded_at, created_at
		FROM videos WHERE status = 'downloaded'
		ORDER BY COALESCE(downloaded_at, created_at) ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

func (s *Store) UpdateVideoStatus(ctx context.Context, id int64, status VideoStatus, errMsg string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE videos SET status = ?, error_message = ? WHERE id = ?`,
		string(status), errMsg, id,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) MarkVideoDownloaded(ctx context.Context, id int64, filePath, thumbPath string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`UPDATE videos SET status = 'downloaded', file_path = ?, thumb_path = ?, error_message = '', downloaded_at = ? WHERE id = ?`,
		filePath, thumbPath, now, id,
	)
	return err
}

func (s *Store) MarkVideoPruned(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE videos SET status = 'pruned', file_path = '', thumb_path = '' WHERE id = ?`,
		id,
	)
	return err
}

// ApproveAllPending approves all videos currently in pending_review status and returns the count updated.
func (s *Store) ApproveAllPending(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE videos SET status = 'approved', error_message = '' WHERE status = 'pending_review'`,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ResetStuckDownloads returns any videos stuck in 'downloading' (e.g. after a crash/restart) back to 'approved'.
func (s *Store) ResetStuckDownloads(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE videos SET status = 'approved' WHERE status = 'downloading'`)
	return err
}

func (s *Store) DeleteVideo(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM videos WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetStats(ctx context.Context) (DashboardStats, error) {
	var st DashboardStats
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM videos GROUP BY status`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return st, err
		}
		switch VideoStatus(status) {
		case StatusPendingReview:
			st.PendingReview = count
		case StatusApproved:
			st.Approved = count
		case StatusDownloading:
			st.Downloading = count
		case StatusDownloaded:
			st.Downloaded = count
		case StatusFailed:
			st.Failed = count
		}
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM channels`).Scan(&st.Channels); err != nil {
		return st, err
	}
	return st, nil
}

// --- helpers ---

type scanner interface {
	Scan(dest ...any) error
}

func scanChannel(sc scanner) (*Channel, error) {
	var ch Channel
	var autoApprove, enabled, skipShorts, skipLive int
	var retDays, retMax sql.NullInt64
	var lastChecked sql.NullTime
	if err := sc.Scan(
		&ch.ID, &ch.YouTubeID, &ch.Name, &ch.URL, &ch.AvatarURL,
		&autoApprove, &enabled, &skipShorts, &skipLive,
		&retDays, &retMax, &lastChecked, &ch.CreatedAt,
	); err != nil {
		return nil, err
	}
	ch.AutoApprove = autoApprove != 0
	ch.Enabled = enabled != 0
	ch.SkipShorts = skipShorts != 0
	ch.SkipLivestreams = skipLive != 0
	if retDays.Valid {
		v := int(retDays.Int64)
		ch.RetentionDays = &v
	}
	if retMax.Valid {
		v := int(retMax.Int64)
		ch.RetentionMaxVideos = &v
	}
	if lastChecked.Valid {
		t := lastChecked.Time
		ch.LastCheckedAt = &t
	}
	return &ch, nil
}

func scanVideo(sc scanner) (*Video, error) {
	var v Video
	var chID sql.NullInt64
	var pubAt, dlAt sql.NullTime
	var isShort, isLive int
	var status string
	if err := sc.Scan(
		&v.ID, &v.YouTubeID, &chID, &v.ChannelName, &v.Title, &v.Description,
		&v.ThumbnailURL, &v.DurationSeconds, &pubAt, &isShort, &isLive, &status,
		&v.FilePath, &v.ThumbPath, &v.ErrorMessage, &dlAt, &v.CreatedAt,
	); err != nil {
		return nil, err
	}
	if chID.Valid {
		id := chID.Int64
		v.ChannelID = &id
	}
	if pubAt.Valid {
		t := pubAt.Time
		v.PublishedAt = &t
	}
	if dlAt.Valid {
		t := dlAt.Time
		v.DownloadedAt = &t
	}
	v.IsShort = isShort != 0
	v.IsLivestream = isLive != 0
	v.Status = VideoStatus(status)
	return &v, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
