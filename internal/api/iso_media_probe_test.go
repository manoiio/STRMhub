package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"strmhub/internal/config"
	"strmhub/internal/model"
)

func TestISOMediaProbeRangeReader(t *testing.T) {
	data := []byte("0123456789abcdefghijklmnopqrstuv")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		value := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
		var start, end int
		if _, err := fmt.Sscanf(value, "%d-%d", &start, &end); err != nil ||
			start < 0 || end >= len(data) || start > end {
			t.Errorf("invalid range: %q", value)
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	defer server.Close()

	reader := &isoRangeReader{
		size: int64(len(data)), blockSize: 8, blocks: make(map[int64][]byte),
		url: server.URL, expires: time.Now().Add(time.Hour), client: server.Client(),
	}
	buf := make([]byte, 11)
	n, err := reader.ReadAt(buf, 5)
	if err != nil || n != len(buf) || !bytes.Equal(buf, data[5:16]) {
		t.Fatalf("cross-block read = %q, %d, %v", buf, n, err)
	}
	again := make([]byte, 4)
	n, err = reader.ReadAt(again, 8)
	if err != nil || n != len(again) || !bytes.Equal(again, data[8:12]) {
		t.Fatalf("cached read = %q, %d, %v", again, n, err)
	}
	if requests != 2 {
		t.Fatalf("got %d HTTP requests, want 2", requests)
	}
	n, err = reader.ReadAt(make([]byte, 4), int64(len(data)-2))
	if n != 2 || err != io.EOF {
		t.Fatalf("end read = %d, %v; want 2, EOF", n, err)
	}
}

func TestISOMediaProbeRejectsNonRangeResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(1024))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	reader := &isoRangeReader{
		size: 1024, blockSize: 8, blocks: make(map[int64][]byte),
		url: server.URL, expires: time.Now().Add(time.Hour), client: server.Client(),
	}
	if _, err := reader.ReadAt(make([]byte, 1), 0); err == nil {
		t.Fatal("full response to a Range request was accepted")
	}
}

func TestISOProbeRefreshesRejectedLink(t *testing.T) {
	rejected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer rejected.Close()
	current := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Range") != "bytes=0-7" {
			t.Error("incorrect renewed range")
		}
		w.Header().Set("Content-Range", "bytes 0-7/8")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("original"))
	}))
	defer current.Close()
	refreshes := 0
	reader := &isoRangeReader{
		size: 8, blockSize: 8, blocks: make(map[int64][]byte), url: rejected.URL, expires: time.Now().Add(time.Hour), client: current.Client(),
		resolve: func() (string, map[string]string, error) { refreshes++; return current.URL, nil, nil },
	}
	data := make([]byte, 8)
	if _, err := reader.ReadAt(data, 0); err != nil || string(data) != "original" || refreshes != 1 {
		t.Fatalf("rejected link was not renewed: %q, refreshes=%d, err=%v", data, refreshes, err)
	}
}

func TestISOProbeDiscoversSizeFromOneByteRange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Range") != "bytes=0-0" {
			t.Error("size discovery requested more than one byte")
		}
		w.Header().Set("Content-Range", "bytes 0-0/49221140480")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte{0})
	}))
	defer server.Close()
	reader := &isoRangeReader{url: server.URL, expires: time.Now().Add(time.Hour), client: server.Client()}
	if err := reader.discoverSize(); err != nil || reader.size != 49221140480 {
		t.Fatalf("size=%d, err=%v", reader.size, err)
	}
}

func TestISOProbeOnlyLeasedLocalLavf(t *testing.T) {
	lease := &isoProbeLease{file: model.SyncedFile{PickCode: "abcdefgh"}, expires: time.Now().Add(time.Minute)}
	isoProbeLeases.Lock()
	isoProbeLeases.active["abcdefgh"] = lease
	isoProbeLeases.Unlock()
	defer finishISOMediaProbe(lease)
	cases := []struct {
		name, peer, ua, pick string
		want                 bool
	}{
		{"local probe", "127.0.0.1:1234", "Lavf/59.27.100", "abcdefgh", true},
		{"local IPv6 probe", "[::1]:1234", "Lavf/59.27.100", "abcdefgh", true},
		{"Infuse during probe", "127.0.0.1:1234", "Infuse/8", "abcdefgh", false},
		{"remote probe", "192.168.1.10:1234", "Lavf/59.27.100", "abcdefgh", false},
		{"other ISO", "127.0.0.1:1234", "Lavf/59.27.100", "ijklmnop", false},
		{"missing peer", "", "Lavf/59.27.100", "abcdefgh", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/d/"+tc.pick, nil)
			request.RemoteAddr = tc.peer
			request.Header.Set("User-Agent", tc.ua)
			request.Header.Set("X-Forwarded-For", "127.0.0.1")
			if got := isoMetadataRequestLease(request, tc.pick) != nil; got != tc.want {
				t.Fatalf("leased=%v, want %v", got, tc.want)
			}
		})
	}
	finishISOMediaProbe(lease)
	request := httptest.NewRequest(http.MethodGet, "/d/abcdefgh", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("User-Agent", "Lavf/59.27.100")
	if isoMetadataRequestLease(request, "abcdefgh") != nil {
		t.Fatal("finished lease still serves metadata")
	}
}

func TestISOProbeRetainsDirectSTRM(t *testing.T) {
	root := t.TempDir()
	file := remoteFile{Fid: "new-iso", Name: "New.Movie.iso", Path: "Movie", PickCode: "abcdefgh"}
	if err := writeStrm(root, "http://127.0.0.1:6086", "pick_code_name", true, false, file); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(root, "Movie", file.Name+".strm"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "/d/abcdefgh.iso") || directURLContainer(string(contents)) != "iso" {
		t.Fatal("new ISO STRM no longer uses original ISO direct-link route")
	}
}

// Opt-in integration check against the configured 115 account. It reads only
// small ranges of an approved ISO and never downloads the full disc.
func TestISOMediaProbeLive(t *testing.T) {
	if os.Getenv("STRMHUB_ISO_PROBE_LIVE") != "1" {
		t.Skip("set STRMHUB_ISO_PROBE_LIVE=1 to probe an approved remote ISO")
	}
	runtimeDir := os.Getenv("STRMHUB_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = filepath.Join("..", "..", ".runtime")
	}
	db, err := gorm.Open(sqlite.Open(filepath.Join(runtimeDir, "data", "strmhub.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: filepath.Join(runtimeDir, "config"), DataDir: filepath.Join(runtimeDir, "data")}
	var full struct {
		LocalPath string `json:"local_path"`
	}
	if err := json.Unmarshal([]byte(cfg.GetSetting("full")), &full); err != nil {
		t.Fatal(err)
	}
	var file model.SyncedFile
	if err := db.Where("kind = ? AND LOWER(rel_path) LIKE ?", "video", "%.iso.strm").First(&file).Error; err != nil {
		t.Fatal(err)
	}
	strmPath := filepath.Join(full.LocalPath, filepath.FromSlash(file.RelPath))
	original, err := os.ReadFile(strmPath)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := beginISOMediaProbe(db, cfg, strmPath)
	if err != nil || lease == nil {
		t.Fatalf("ISO lease setup failed: %v", err)
	}
	defer finishISOMediaProbe(lease)
	source, err := getISOMediaProbeSource(db, cfg, file)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := source.main.NewReader()
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 192)
	if _, err := io.ReadFull(reader, buf); err != nil {
		t.Fatal(err)
	}
	if buf[4] != 0x47 {
		t.Fatal("main clip did not begin with an M2TS packet")
	}
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.GET("/d/:pickcode", func(c *gin.Context) { servePickcodeDirect(c, db, cfg, c.Param("pickcode")) })
	router.GET("/iso-media/:pickcode/main.m2ts", func(c *gin.Context) {
		if !serveISOMetadataIfLeased(c, db, cfg, c.Param("pickcode")) {
			c.Status(404)
		}
	})
	server := httptest.NewServer(router)
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/iso-media/"+file.PickCode+"/main.m2ts", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Range", "bytes=0-191")
	request.Header.Set("User-Agent", "Lavf/59.27.100")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusPartialContent || !bytes.Equal(body, buf) {
		t.Fatalf("bridge range response: HTTP %d, %d bytes", response.StatusCode, len(body))
	}
	cacheKey := file.PickCode + "|Infuse/8"
	downloadCacheMu.Lock()
	downloadLinkCache[cacheKey] = downloadCacheEntry{URL: "https://example.invalid/original.iso", Expiry: time.Now().Add(time.Minute)}
	downloadCacheMu.Unlock()
	defer func() { downloadCacheMu.Lock(); delete(downloadLinkCache, cacheKey); downloadCacheMu.Unlock() }()
	infuseRequest, _ := http.NewRequest(http.MethodGet, server.URL+"/d/"+file.PickCode, nil)
	infuseRequest.Header.Set("User-Agent", "Infuse/8")
	client := server.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	direct, err := client.Do(infuseRequest)
	if err != nil {
		t.Fatal(err)
	}
	direct.Body.Close()
	if direct.StatusCode != http.StatusFound || direct.Header.Get("Location") != "https://example.invalid/original.iso" {
		t.Fatalf("Infuse did not keep ISO redirect: HTTP %d", direct.StatusCode)
	}
	after, err := os.ReadFile(strmPath)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatal("metadata probe changed ISO STRM")
	}
	t.Logf("main clip metadata ready: %d bytes; Infuse keeps ISO redirect; STRM unchanged", source.main.Size())
}

// This opt-in integration imports metadata into the explicitly selected item.
func TestISOMediaProbeEmbyLive(t *testing.T) {
	if os.Getenv("STRMHUB_ISO_PROBE_EMBY_LIVE") != "1" {
		t.Skip("explicit live ISO import opt-in required")
	}
	itemID := os.Getenv("STRMHUB_ISO_PROBE_ITEM_ID")
	numericID, err := strconv.ParseInt(itemID, 10, 64)
	if err != nil {
		t.Fatal("explicit item ID required")
	}
	runtimeDir := filepath.Join("..", "..", ".runtime")
	db, err := gorm.Open(sqlite.Open(filepath.Join(runtimeDir, "data", "strmhub.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: filepath.Join(runtimeDir, "config"), DataDir: filepath.Join(runtimeDir, "data")}
	handler := &Handler{DB: db, Config: cfg}
	base, key, ok := handler.embyServerInfo()
	if !ok {
		t.Fatal("Emby not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 120 * time.Second}
	var page embyProbeItemsPage
	status, err := embyProbeRequest(ctx, client, base, key, "/Items?Ids="+itemID+"&Fields=Path,MediaSources", &page)
	if err != nil || status != 200 || len(page.Items) != 1 {
		t.Fatalf("item lookup: %d %v", status, err)
	}
	original := page.Items[0]
	originalBytes, err := os.ReadFile(original.Path)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := beginISOMediaProbe(db, cfg, original.Path)
	if err != nil || lease == nil {
		t.Fatalf("lease: %v", err)
	}
	defer finishISOMediaProbe(lease)
	router := gin.New()
	router.GET("/iso-media/:pickcode/main.m2ts", func(c *gin.Context) {
		if !serveISOMetadataIfLeased(c, db, cfg, c.Param("pickcode")) {
			c.Status(404)
		}
	})
	server := httptest.NewServer(router)
	defer server.Close()
	var result struct {
		ItemID          int64 `json:"ItemId"`
		VideoStreams    int
		AudioStreams    int
		SubtitleStreams int
		Container       string
	}
	status, err = embyProbeHTTP(ctx, client, base, key, http.MethodPost, "/STRMhub/IsoMediaInfo", map[string]any{"Id": numericID, "IsoSize": lease.file.Size, "ProbeUrl": server.URL + "/iso-media/" + lease.file.PickCode + "/main.m2ts"}, &result)
	if err != nil || status != 200 || result.ItemID != numericID || result.VideoStreams < 1 {
		t.Fatalf("native import: HTTP %d err=%v", status, err)
	}
	isoProbeLeases.Lock()
	requests := lease.requests
	isoProbeLeases.Unlock()
	if requests == 0 {
		t.Fatal("Emby did not probe metadata")
	}
	after, err := os.ReadFile(original.Path)
	if err != nil || !bytes.Equal(originalBytes, after) {
		t.Fatal("original ISO STRM changed")
	}
	var saved embyProbeItemsPage
	status, err = embyProbeRequest(ctx, client, base, key, "/Items?Ids="+itemID+"&Fields=Path,MediaSources", &saved)
	if err != nil || status != 200 || len(saved.Items) != 1 || embyItemMissingAV(saved.Items[0]) || saved.Items[0].Path != original.Path {
		t.Fatal("native media information was not saved")
	}
	t.Logf("saved ISO metadata: video=%d audio=%d subtitles=%d container=%s requests=%d; original STRM unchanged", result.VideoStreams, result.AudioStreams, result.SubtitleStreams, result.Container, requests)
}
