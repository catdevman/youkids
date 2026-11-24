package plex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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

// ParseLabels splits a comma-separated label string into trimmed, non-empty label names.
func ParseLabels(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ApplyLabelsToSection ensures every item in the given Plex library section has all specified labels
// applied and locked (e.g. "Kids" for Plex Home managed profile label restrictions).
// Returns the number of items updated.
func (c *Client) ApplyLabelsToSection(ctx context.Context, plexURL, token, sectionID string, labels []string) (int, error) {
	plexURL = strings.TrimRight(strings.TrimSpace(plexURL), "/")
	token = strings.TrimSpace(token)
	sectionID = strings.TrimSpace(sectionID)
	if plexURL == "" || token == "" || sectionID == "" || len(labels) == 0 {
		return 0, nil
	}

	listURL := fmt.Sprintf("%s/library/sections/%s/all", plexURL, sectionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Plex-Token", token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("plex list section items: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("plex list section items returned HTTP %d", resp.StatusCode)
	}

	var payload struct {
		MediaContainer struct {
			Metadata []struct {
				RatingKey string `json:"ratingKey"`
				Type      string `json:"type"`
				Label     []struct {
					Tag string `json:"tag"`
				} `json:"Label"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, fmt.Errorf("decode plex section items: %w", err)
	}

	updated := 0
	for _, item := range payload.MediaContainer.Metadata {
		existing := make(map[string]bool, len(item.Label))
		for _, l := range item.Label {
			existing[strings.ToLower(strings.TrimSpace(l.Tag))] = true
		}

		var missing []string
		for _, want := range labels {
			if !existing[strings.ToLower(want)] {
				missing = append(missing, want)
			}
		}
		if len(missing) == 0 {
			continue
		}

		q := url.Values{}
		q.Set("type", plexTypeID(item.Type))
		q.Set("id", item.RatingKey)
		q.Set("label.locked", "1")
		for _, lbl := range missing {
			q.Add("label[].tag.tag", lbl)
		}

		putURL := fmt.Sprintf("%s/library/sections/%s/all?%s", plexURL, sectionID, q.Encode())
		putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, putURL, nil)
		if err != nil {
			return updated, err
		}
		putReq.Header.Set("X-Plex-Token", token)
		putReq.Header.Set("Accept", "application/json")

		putResp, err := c.HTTP.Do(putReq)
		if err != nil {
			return updated, fmt.Errorf("plex label item %s: %w", item.RatingKey, err)
		}
		putResp.Body.Close()
		if putResp.StatusCode < 200 || putResp.StatusCode >= 300 {
			return updated, fmt.Errorf("plex label item %s returned HTTP %d", item.RatingKey, putResp.StatusCode)
		}
		updated++
	}
	return updated, nil
}

func plexTypeID(t string) string {
	switch strings.ToLower(t) {
	case "show":
		return "2"
	case "season":
		return "3"
	case "episode":
		return "4"
	default:
		// Plex "Other Videos" items use type "movie" (1).
		return "1"
	}
}
