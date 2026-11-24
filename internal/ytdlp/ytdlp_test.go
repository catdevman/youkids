package ytdlp

import "testing"

func TestNormalizeChannelURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"UC1234567890123456789012", "https://www.youtube.com/channel/UC1234567890123456789012"},
		{"@SciShowKids", "https://www.youtube.com/@SciShowKids"},
		{"SciShowKids", "https://www.youtube.com/@SciShowKids"},
		{"https://www.youtube.com/@SciShowKids/", "https://www.youtube.com/@SciShowKids"},
	}
	for _, tc := range cases {
		if got := NormalizeChannelURL(tc.in); got != tc.want {
			t.Errorf("NormalizeChannelURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestChannelVideosURL(t *testing.T) {
	if got := ChannelVideosURL("https://www.youtube.com/@SciShowKids"); got != "https://www.youtube.com/@SciShowKids/videos" {
		t.Fatalf("got %q", got)
	}
	if got := ChannelVideosURL("https://www.youtube.com/@SciShowKids/videos"); got != "https://www.youtube.com/@SciShowKids/videos" {
		t.Fatalf("got %q", got)
	}
}

func TestSanitizeFilename(t *testing.T) {
	got := SanitizeFilename(`  Why: Do <Cats> Purr? / "Episode 1"  `)
	want := "Why Do Cats Purr Episode 1"
	if got != want {
		t.Fatalf("SanitizeFilename() = %q, want %q", got, want)
	}
	if got := SanitizeFilename("..."); got != "Untitled" {
		t.Fatalf("expected Untitled, got %q", got)
	}
}

func TestParseVideoJSON(t *testing.T) {
	shortJSON := []byte(`{"id":"s1","title":"Quick Short","url":"https://www.youtube.com/shorts/s1","duration":45,"live_status":"not_live"}`)
	v, ok := parseVideoJSON(shortJSON)
	if !ok || !v.IsShort || v.IsLivestream {
		t.Fatalf("expected short=true live=false, got %+v (ok=%v)", v, ok)
	}

	liveJSON := []byte(`{"id":"l1","title":"Live Stream","duration":3600,"live_status":"is_live","is_live":true}`)
	v2, ok := parseVideoJSON(liveJSON)
	if !ok || !v2.IsLivestream {
		t.Fatalf("expected livestream=true, got %+v (ok=%v)", v2, ok)
	}

	normalJSON := []byte(`{"id":"n1","title":"Normal Episode","duration":420,"upload_date":"20261120","channel":"SciShow Kids"}`)
	v3, ok := parseVideoJSON(normalJSON)
	if !ok || v3.IsShort || v3.IsLivestream || v3.PublishedAt == nil {
		t.Fatalf("expected normal video with published_at, got %+v (ok=%v)", v3, ok)
	}
}
