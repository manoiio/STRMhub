package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestEmbyItemMissingAV(t *testing.T) {
	cases := []struct {
		name string
		item embyProbeItem
		want bool
	}{
		{"subtitle only", embyProbeItem{ID: "1", Path: "/anime/S01E01.mkv.strm", MediaSources: []embyProbeSource{{MediaStreams: []embyProbeStream{{Type: "Subtitle"}}}}}, true},
		{"empty streams", embyProbeItem{ID: "2", Path: "/anime/S01E02.MKV.STRM"}, true},
		{"video present", embyProbeItem{ID: "3", Path: "/anime/S01E03.strm", MediaSources: []embyProbeSource{{MediaStreams: []embyProbeStream{{Type: "Video"}}}}}, false},
		{"audio present", embyProbeItem{ID: "4", Path: "/anime/S01E04.strm", MediaSources: []embyProbeSource{{MediaStreams: []embyProbeStream{{Type: "Audio"}}}}}, false},
		{"own version missing", embyProbeItem{ID: "6", Path: "/anime/S01E06.strm", MediaSources: []embyProbeSource{{ItemID: "other", MediaStreams: []embyProbeStream{{Type: "Video"}}}, {ItemID: "6", MediaStreams: []embyProbeStream{{Type: "Subtitle"}}}}}, true},
		{"own version ready", embyProbeItem{ID: "7", Path: "/anime/S01E07.strm", MediaSources: []embyProbeSource{{ItemID: "7", MediaStreams: []embyProbeStream{{Type: "Video"}}}, {ItemID: "other", MediaStreams: []embyProbeStream{{Type: "Subtitle"}}}}}, false},
		{"local video", embyProbeItem{ID: "5", Path: "/anime/S01E05.mkv"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := embyItemMissingAV(tc.item); got != tc.want {
				t.Fatalf("missing AV = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEmbyProbeResponseHasAVMatchesItemID(t *testing.T) {
	sources := []embyProbeSource{
		{ItemID: "other", MediaStreams: []embyProbeStream{{Type: "Video"}}},
		{ItemID: "target", MediaStreams: []embyProbeStream{{Type: "Subtitle"}}},
	}
	if embyProbeResponseHasAV(sources, "target") {
		t.Fatal("other version's video must not satisfy target")
	}
	sources[1].MediaStreams = append(sources[1].MediaStreams, embyProbeStream{Type: "Audio"})
	if !embyProbeResponseHasAV(sources, "target") {
		t.Fatal("target audio should satisfy target despite other versions")
	}
}

func TestEmbyProbeCandidatesIncludesSubtitleOnlyItems(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Emby-Token") != "test-key" {
			t.Error("missing API key header")
		}
		if r.URL.Path != "/emby/Items" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		start, _ := strconv.Atoi(r.URL.Query().Get("StartIndex"))
		items := []embyProbeItem{
			{ID: "1", Path: "/anime/one.strm", MediaSources: []embyProbeSource{{ID: "source-one", ItemID: "1", MediaStreams: []embyProbeStream{{Type: "Subtitle"}}}}},
			{ID: "2", Path: "/anime/two.strm", MediaSources: []embyProbeSource{{MediaStreams: []embyProbeStream{{Type: "Video"}}}}},
			{ID: "3", Path: "/anime/three.strm"},
		}
		if start > len(items) {
			start = len(items)
		}
		json.NewEncoder(w).Encode(embyProbeItemsPage{Items: items[start:], TotalRecordCount: len(items)})
	}))
	defer server.Close()
	candidates, err := embyProbeCandidates(context.Background(), server.Client(), server.URL, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || candidates[0].ItemID != "1" || candidates[0].MediaSourceID != "source-one" || candidates[1].ItemID != "3" || candidates[1].MediaSourceID != "mediasource_3" {
		t.Fatalf("candidates = %v, want source-one and mediasource_3", candidates)
	}
}

func TestEmbyProbeHTTPPostsSelectedMediaSource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/emby/Items/8383/PlaybackInfo" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Emby-Token") != "test-key" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("missing request headers")
		}
		var body struct {
			ID            string `json:"Id"`
			UserID        string `json:"UserId"`
			MediaSourceID string `json:"MediaSourceId"`
			IsPlayback    bool   `json:"IsPlayback"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.ID != "8383" || body.UserID != "user" || body.MediaSourceID != "mediasource_8383" || !body.IsPlayback {
			t.Errorf("unexpected playback body: %+v", body)
		}
		json.NewEncoder(w).Encode(map[string]any{"MediaSources": []embyProbeSource{{ItemID: "8383", MediaStreams: []embyProbeStream{{Type: "Video"}}}}})
	}))
	defer server.Close()
	var response struct {
		MediaSources []embyProbeSource `json:"MediaSources"`
	}
	payload := map[string]any{"Id": "8383", "UserId": "user", "MediaSourceId": "mediasource_8383", "IsPlayback": true}
	status, err := embyProbeHTTP(context.Background(), server.Client(), server.URL, "test-key", http.MethodPost, "/Items/8383/PlaybackInfo", payload, &response)
	if err != nil || status != http.StatusOK || !embyProbeResponseHasAV(response.MediaSources, "8383") {
		t.Fatalf("selected source probe: status=%d, err=%v, sources=%v", status, err, response.MediaSources)
	}
}
