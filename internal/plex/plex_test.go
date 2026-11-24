package plex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseLabels(t *testing.T) {
	got := ParseLabels(" Kids , , YouTube , ")
	if len(got) != 2 || got[0] != "Kids" || got[1] != "YouTube" {
		t.Fatalf("unexpected labels: %#v", got)
	}
}

func TestApplyLabelsToSection(t *testing.T) {
	var labeledIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/library/sections/4/all" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"MediaContainer": {
					"Metadata": [
						{"ratingKey": "101", "type": "movie", "Label": [{"tag": "Kids"}]},
						{"ratingKey": "102", "type": "movie", "Label": []}
					]
				}
			}`))
			return
		}
		if r.Method == http.MethodPut && r.URL.Path == "/library/sections/4/all" {
			q := r.URL.Query()
			if q.Get("type") != "1" || q.Get("label.locked") != "1" || q.Get("label[].tag.tag") != "Kids" {
				t.Errorf("unexpected PUT query: %s", r.URL.RawQuery)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			labeledIDs = append(labeledIDs, q.Get("id"))
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient()
	updated, err := c.ApplyLabelsToSection(context.Background(), srv.URL, "tok", "4", []string{"Kids"})
	if err != nil {
		t.Fatalf("ApplyLabelsToSection: %v", err)
	}
	if updated != 1 || len(labeledIDs) != 1 || labeledIDs[0] != "102" {
		t.Fatalf("expected only item 102 to be labeled, got updated=%d ids=%v", updated, labeledIDs)
	}
}
