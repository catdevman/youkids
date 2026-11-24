package plex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Section represents a Plex library section.
type Section struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Type  string `json:"type"`
}

// Client communicates with a Plex Media Server instance.
type Client struct {
	HTTP *http.Client
}

// NewClient creates a Plex API client.
func NewClient() *Client {
	return &Client{
		HTTP: &http.Client{Timeout: 10 * time.Second},
	}
}

// RefreshLibrary triggers a Plex library scan for the given section ID (or all sections if empty).
func (c *Client) RefreshLibrary(ctx context.Context, plexURL, token, sectionID string) error {
	plexURL = strings.TrimRight(strings.TrimSpace(plexURL), "/")
	token = strings.TrimSpace(token)
	if plexURL == "" || token == "" {
		return nil
	}

	sectionID = strings.TrimSpace(sectionID)
	if sectionID == "" {
		sectionID = "all"
	}
	endpoint := fmt.Sprintf("%s/library/sections/%s/refresh", plexURL, sectionID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Plex-Token", token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("plex refresh request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("plex refresh returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// ListSections fetches all library sections from Plex so the parent can select their Kids library.
func (c *Client) ListSections(ctx context.Context, plexURL, token string) ([]Section, error) {
	plexURL = strings.TrimRight(strings.TrimSpace(plexURL), "/")
	token = strings.TrimSpace(token)
	if plexURL == "" || token == "" {
		return nil, fmt.Errorf("plex URL and token are required")
	}

	endpoint := fmt.Sprintf("%s/library/sections", plexURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Plex-Token", token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("plex list sections: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("plex returned HTTP %d", resp.StatusCode)
	}

	var payload struct {
		MediaContainer struct {
			Directory []Section `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode plex sections: %w", err)
	}
	return payload.MediaContainer.Directory, nil
}
