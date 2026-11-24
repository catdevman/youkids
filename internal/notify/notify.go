package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/catdevman/youkids/internal/db"
)

// Notifier dispatches parent review alerts to configured push channels.
type Notifier struct {
	HTTP *http.Client
}

// NewNotifier creates a Notifier with sensible HTTP timeouts.
func NewNotifier() *Notifier {
	return &Notifier{
		HTTP: &http.Client{Timeout: 10 * time.Second},
	}
}

// NotifyPendingReview sends a consolidated notification when new videos await parent approval.
func (n *Notifier) NotifyPendingReview(ctx context.Context, cfg db.Settings, videos []db.Video) error {
	if len(videos) == 0 {
		return nil
	}

	title := fmt.Sprintf("YouKids: %d new video(s) awaiting review", len(videos))
	var lines []string
	maxShow := 6
	for i, v := range videos {
		if i >= maxShow {
			lines = append(lines, fmt.Sprintf("...and %d more", len(videos)-maxShow))
			break
		}
		lines = append(lines, fmt.Sprintf("• [%s] %s", v.ChannelName, v.Title))
	}
	reviewURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if reviewURL != "" {
		lines = append(lines, "", "Review queue: "+reviewURL)
	}
	body := strings.Join(lines, "\n")

	return n.dispatchAll(ctx, cfg, "pending_review", title, body, reviewURL, videos)
}

// NotifyStorageHardLimit alerts the parent when downloads are paused due to the hard storage cap.
func (n *Notifier) NotifyStorageHardLimit(ctx context.Context, cfg db.Settings, usedBytes, hardLimitBytes int64) error {
	usedGB := float64(usedBytes) / (1024 * 1024 * 1024)
	limitGB := float64(hardLimitBytes) / (1024 * 1024 * 1024)
	title := "YouKids: Downloads paused (storage hard limit reached)"
	body := fmt.Sprintf("Media storage is at %.2f GB / %.2f GB. New downloads are paused until space is freed or limits are increased.", usedGB, limitGB)
	reviewURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if reviewURL != "" {
		body += "\n\nOpen YouKids: " + reviewURL
	}
	return n.dispatchAll(ctx, cfg, "storage_hard_limit", title, body, reviewURL, nil)
}

// SendTest sends a test notification to all configured notification providers.
func (n *Notifier) SendTest(ctx context.Context, cfg db.Settings) (int, error) {
	reviewURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	sent, errs := n.dispatchWithCount(
		ctx, cfg, "test",
		"YouKids Test Notification",
		"Your YouKids notification channel is working! You will be notified here when new videos await review.",
		reviewURL,
		nil,
	)
	if len(errs) > 0 {
		return sent, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return sent, nil
}

func (n *Notifier) dispatchAll(ctx context.Context, cfg db.Settings, event, title, body, clickURL string, videos []db.Video) error {
	_, errs := n.dispatchWithCount(ctx, cfg, event, title, body, clickURL, videos)
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func (n *Notifier) dispatchWithCount(ctx context.Context, cfg db.Settings, event, title, body, clickURL string, videos []db.Video) (int, []string) {
	var sent int
	var errs []string

	if cfg.NotifyNtfyTopic != "" {
		sent++
		if err := n.sendNtfy(ctx, cfg, title, body, clickURL); err != nil {
			errs = append(errs, "ntfy: "+err.Error())
		}
	}
	if cfg.NotifyDiscordWebhook != "" {
		sent++
		if err := n.sendDiscord(ctx, cfg.NotifyDiscordWebhook, title, body, clickURL); err != nil {
			errs = append(errs, "discord: "+err.Error())
		}
	}
	if cfg.NotifyTelegramBotToken != "" && cfg.NotifyTelegramChatID != "" {
		sent++
		if err := n.sendTelegram(ctx, cfg.NotifyTelegramBotToken, cfg.NotifyTelegramChatID, title, body); err != nil {
			errs = append(errs, "telegram: "+err.Error())
		}
	}
	if cfg.NotifyGenericWebhookURL != "" {
		sent++
		if err := n.sendGenericWebhook(ctx, cfg.NotifyGenericWebhookURL, event, title, body, clickURL, videos); err != nil {
			errs = append(errs, "webhook: "+err.Error())
		}
	}
	return sent, errs
}

func (n *Notifier) sendNtfy(ctx context.Context, cfg db.Settings, title, body, clickURL string) error {
	base := strings.TrimRight(strings.TrimSpace(cfg.NotifyNtfyURL), "/")
	if base == "" {
		base = "https://ntfy.sh"
	}
	endpoint := base + "/" + strings.TrimLeft(strings.TrimSpace(cfg.NotifyNtfyTopic), "/")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", title)
	req.Header.Set("Tags", "tv,kids")
	if clickURL != "" {
		req.Header.Set("Click", clickURL)
	}
	if cfg.NotifyNtfyToken != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(cfg.NotifyNtfyToken))
	}
	return n.do(req)
}

func (n *Notifier) sendDiscord(ctx context.Context, webhookURL, title, body, clickURL string) error {
	embed := map[string]any{
		"title":       title,
		"description": body,
		"color":       3447003,
	}
	if clickURL != "" {
		embed["url"] = clickURL
	}
	payload := map[string]any{
		"username": "YouKids",
		"embeds":   []any{embed},
	}
	return n.postJSON(ctx, webhookURL, payload)
}

func (n *Notifier) sendTelegram(ctx context.Context, botToken, chatID, title, body string) error {
	endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", strings.TrimSpace(botToken))
	payload := map[string]any{
		"chat_id": strings.TrimSpace(chatID),
		"text":    title + "\n\n" + body,
	}
	return n.postJSON(ctx, endpoint, payload)
}

func (n *Notifier) sendGenericWebhook(ctx context.Context, webhookURL, event, title, body, clickURL string, videos []db.Video) error {
	type videoItem struct {
		YouTubeID   string `json:"youtube_id"`
		ChannelName string `json:"channel_name"`
		Title       string `json:"title"`
		URL         string `json:"url"`
	}
	items := make([]videoItem, 0, len(videos))
	for _, v := range videos {
		items = append(items, videoItem{
			YouTubeID:   v.YouTubeID,
			ChannelName: v.ChannelName,
			Title:       v.Title,
			URL:         "https://www.youtube.com/watch?v=" + v.YouTubeID,
		})
	}
	payload := map[string]any{
		"event":   event,
		"title":   title,
		"message": body,
		"url":     clickURL,
		"count":   len(videos),
		"videos":  items,
	}
	return n.postJSON(ctx, webhookURL, payload)
}

func (n *Notifier) postJSON(ctx context.Context, url string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSpace(url), bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return n.do(req)
}

func (n *Notifier) do(req *http.Request) error {
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
