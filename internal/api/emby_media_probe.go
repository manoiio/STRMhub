package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Emby deliberately does not probe remote STRM targets during a library scan.
// The stock StrmExtract task sends requests too quickly for our /d/ rate limit
// and skips items that already have an external subtitle stream. This worker
// uses Emby's API to find items lacking video/audio streams and probes them at
// a measured pace. Emby remains the owner of the resulting media information.
const (
	embyProbeSettingKey = "emby-media-info-probe"
	embyProbePageSize   = 200
	embyProbePace       = 8 * time.Second
	embyProbeRetryDelay = 60 * time.Second
	embyProbeScanDelay  = 30 * time.Minute
	embyProbeFailedTTL  = 2 * time.Hour
)

type embyProbeStream struct {
	Type string `json:"Type"`
}

type embyProbeSource struct {
	ID           string            `json:"Id"`
	ItemID       string            `json:"ItemId"`
	Path         string            `json:"Path"`
	MediaStreams []embyProbeStream `json:"MediaStreams"`
}

type embyProbeCandidate struct {
	ItemID        string
	MediaSourceID string
	Path          string
}

type embyProbeItem struct {
	ID           string            `json:"Id"`
	Path         string            `json:"Path"`
	IsFolder     bool              `json:"IsFolder"`
	MediaSources []embyProbeSource `json:"MediaSources"`
}

type embyProbeItemsPage struct {
	Items            []embyProbeItem `json:"Items"`
	TotalRecordCount int             `json:"TotalRecordCount"`
}

type embyProbeUser struct {
	ID     string `json:"Id"`
	Policy struct {
		IsAdministrator bool `json:"IsAdministrator"`
		IsDisabled      bool `json:"IsDisabled"`
	} `json:"Policy"`
}

type embyProbeStatus struct {
	Running   bool   `json:"running"`
	Total     int    `json:"total"`
	Done      int    `json:"done"`
	Succeeded int    `json:"succeeded"`
	Failed    int    `json:"failed"`
	Deferred  int    `json:"deferred"`
	LastError string `json:"last_error,omitempty"`
	LastRun   string `json:"last_run,omitempty"`
}

var (
	embyProbeMu      sync.Mutex
	embyProbeCurrent embyProbeStatus
	embyProbeRetryAt = map[string]time.Time{}
)

func (h *Handler) embyProbeEnabled() bool {
	var setting struct {
		Enabled bool `json:"enabled"`
	}
	return json.Unmarshal([]byte(h.getSettingValue(embyProbeSettingKey)), &setting) == nil && setting.Enabled
}

func (h *Handler) EmbyMediaProbeStatus(c *gin.Context) {
	embyProbeMu.Lock()
	status := embyProbeCurrent
	embyProbeMu.Unlock()
	c.JSON(http.StatusOK, gin.H{"enabled": h.embyProbeEnabled(), "status": status})
}

func updateEmbyProbeStatus(change func(*embyProbeStatus)) {
	embyProbeMu.Lock()
	change(&embyProbeCurrent)
	embyProbeMu.Unlock()
}

func embyItemMissingAV(item embyProbeItem) bool {
	if item.IsFolder || item.ID == "" || !strings.HasSuffix(strings.ToLower(item.Path), ".strm") {
		return false
	}
	return !embyProbeResponseHasAV(item.MediaSources, item.ID)
}

func embyProbeRequest(ctx context.Context, client *http.Client, base, key, path string, out any) (int, error) {
	return embyProbeHTTP(ctx, client, base, key, http.MethodGet, path, nil, out)
}

func embyProbeHTTP(ctx context.Context, client *http.Client, base, key, method, path string, payload any, out any) (int, error) {
	base = strings.TrimRight(base, "/")
	if !strings.HasSuffix(strings.ToLower(base), "/emby") {
		base += "/emby"
	}
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Emby-Token", key)
	req.Header.Set("User-Agent", "STRMhub-MediaInfo-Probe/1")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		return resp.StatusCode, nil
	}
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		return resp.StatusCode, nil
	}
	return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out)
}

func embyProbeUserID(ctx context.Context, client *http.Client, base, key string) (string, error) {
	var users []embyProbeUser
	status, err := embyProbeRequest(ctx, client, base, key, "/Users", &users)
	if err != nil || status != http.StatusOK {
		return "", fmt.Errorf("Emby 用户查询失败: HTTP %d: %v", status, err)
	}
	for _, user := range users {
		if !user.Policy.IsDisabled && user.Policy.IsAdministrator {
			return user.ID, nil
		}
	}
	for _, user := range users {
		if !user.Policy.IsDisabled {
			return user.ID, nil
		}
	}
	return "", fmt.Errorf("Emby 没有可用用户")
}

func embyProbeCandidates(ctx context.Context, client *http.Client, base, key string) ([]embyProbeCandidate, error) {
	var candidates []embyProbeCandidate
	for start := 0; ; start += embyProbePageSize {
		var page embyProbeItemsPage
		path := "/Items?Recursive=true&Fields=Path,MediaSources&Limit=" + strconv.Itoa(embyProbePageSize) + "&StartIndex=" + strconv.Itoa(start)
		status, err := embyProbeRequest(ctx, client, base, key, path, &page)
		if err != nil || status != http.StatusOK {
			return nil, fmt.Errorf("Emby 媒体列表查询失败: HTTP %d: %v", status, err)
		}
		for _, item := range page.Items {
			if embyItemMissingAV(item) {
				source, found := embyProbeSourceForItem(item.MediaSources, item.ID)
				mediaSourceID := "mediasource_" + item.ID
				if found && source.ID != "" {
					mediaSourceID = source.ID
				}
				candidatePath := item.Path
				if found && strings.HasSuffix(strings.ToLower(source.Path), ".strm") {
					candidatePath = source.Path
				}
				candidates = append(candidates, embyProbeCandidate{ItemID: item.ID, MediaSourceID: mediaSourceID, Path: candidatePath})
			}
		}
		if len(page.Items) < embyProbePageSize || (page.TotalRecordCount > 0 && start+len(page.Items) >= page.TotalRecordCount) {
			break
		}
	}
	return candidates, nil
}

func embyProbeResponseHasAV(sources []embyProbeSource, itemID string) bool {
	source, found := embyProbeSourceForItem(sources, itemID)
	if !found {
		return false
	}
	for _, stream := range source.MediaStreams {
		if strings.EqualFold(stream.Type, "Video") || strings.EqualFold(stream.Type, "Audio") {
			return true
		}
	}
	return false
}

func embyProbeSourceForItem(sources []embyProbeSource, itemID string) (embyProbeSource, bool) {
	for _, source := range sources {
		if source.ItemID == itemID {
			return source, true
		}
	}
	// A single unlabelled media source is also valid for older Emby responses.
	if len(sources) == 1 && sources[0].ItemID == "" {
		return sources[0], true
	}
	return embyProbeSource{}, false
}

func waitEmbyProbe(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (h *Handler) runEmbyMediaProbe(ctx context.Context) {
	base, key, ok := h.embyServerInfo()
	if !ok || key == "" {
		updateEmbyProbeStatus(func(s *embyProbeStatus) { s.LastError = "Emby 地址或 API 密钥未配置" })
		return
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	userID, err := embyProbeUserID(ctx, client, base, key)
	if err != nil {
		updateEmbyProbeStatus(func(s *embyProbeStatus) { s.LastError = err.Error() })
		log.Printf("[Emby媒体信息] ○ %v", err)
		return
	}
	candidates, err := embyProbeCandidates(ctx, client, base, key)
	if err != nil {
		updateEmbyProbeStatus(func(s *embyProbeStatus) { s.LastError = err.Error() })
		log.Printf("[Emby媒体信息] ○ %v", err)
		return
	}
	embyProbeMu.Lock()
	now := time.Now()
	ready := candidates[:0]
	for _, candidate := range candidates {
		if retryAt, ok := embyProbeRetryAt[candidate.ItemID]; !ok || !now.Before(retryAt) {
			ready = append(ready, candidate)
		}
	}
	deferred := len(candidates) - len(ready)
	candidates = ready
	embyProbeMu.Unlock()
	updateEmbyProbeStatus(func(s *embyProbeStatus) {
		*s = embyProbeStatus{Running: true, Total: len(candidates), Deferred: deferred, LastRun: time.Now().Format(time.RFC3339)}
	})
	log.Printf("[Emby媒体信息] 开始补全：待探测 %d 项，暂缓重试 %d 项，间隔 %s", len(candidates), deferred, embyProbePace)
	defer func() {
		updateEmbyProbeStatus(func(s *embyProbeStatus) { s.Running = false })
		log.Printf("[Emby媒体信息] 本轮结束")
	}()
	consecutiveServiceFailures := 0
	for _, candidate := range candidates {
		if ctx.Err() != nil || !h.embyProbeEnabled() {
			return
		}
		id := candidate.ItemID
		var result struct {
			MediaSources []embyProbeSource `json:"MediaSources"`
		}
		path := "/Items/" + url.PathEscape(id) + "/PlaybackInfo"
		payload := struct {
			ID            string `json:"Id"`
			UserID        string `json:"UserId"`
			MediaSourceID string `json:"MediaSourceId"`
			IsPlayback    bool   `json:"IsPlayback"`
		}{ID: id, UserID: userID, MediaSourceID: candidate.MediaSourceID, IsPlayback: true}
		lease, reqErr := beginISOMediaProbe(h.DB, h.Config, candidate.Path)
		prepareErr := reqErr
		status := 0
		success := false
		if reqErr == nil && lease != nil {
			var isoResult struct {
				ItemID       int64 `json:"ItemId"`
				VideoStreams int   `json:"VideoStreams"`
			}
			itemID, parseErr := strconv.ParseInt(id, 10, 64)
			if parseErr != nil {
				reqErr = parseErr
			} else {
				probeURL := fmt.Sprintf("http://127.0.0.1:%d/iso-media/%s/main.m2ts", h.Config.ProxyPort, lease.file.PickCode)
				isoPayload := map[string]interface{}{"Id": itemID, "ProbeUrl": probeURL, "IsoSize": lease.file.Size}
				status, reqErr = embyProbeHTTP(ctx, client, base, key, http.MethodPost, "/STRMhub/IsoMediaInfo", isoPayload, &isoResult)
				success = reqErr == nil && status == http.StatusOK && isoResult.ItemID == itemID && isoResult.VideoStreams > 0
			}
		} else if reqErr == nil {
			status, reqErr = embyProbeHTTP(ctx, client, base, key, http.MethodPost, path, payload, &result)
			success = reqErr == nil && status == http.StatusOK && embyProbeResponseHasAV(result.MediaSources, id)
		}
		isoFailure := finishISOMediaProbe(lease)
		failureDetail := fmt.Sprintf("HTTP %d", status)
		if reqErr != nil {
			failureDetail = reqErr.Error()
		} else if status == http.StatusOK && !success {
			failureDetail = "未返回该版本的视频或音轨"
		}
		if !success && isoFailure != "" {
			failureDetail = isoFailure
		}
		updateEmbyProbeStatus(func(s *embyProbeStatus) {
			s.Done++
			if success {
				s.Succeeded++
				s.LastError = ""
			} else {
				s.Failed++
				s.LastError = fmt.Sprintf("条目 %s 探测失败（%s）", id, failureDetail)
			}
		})
		if success {
			consecutiveServiceFailures = 0
			embyProbeMu.Lock()
			delete(embyProbeRetryAt, id)
			embyProbeMu.Unlock()
		} else {
			serviceFailure := (reqErr != nil && prepareErr == nil) || status == http.StatusTooManyRequests || status >= http.StatusInternalServerError || status == http.StatusUnauthorized || status == http.StatusForbidden
			embyProbeMu.Lock()
			embyProbeRetryAt[id] = time.Now().Add(embyProbeFailedTTL)
			embyProbeMu.Unlock()
			log.Printf("[Emby媒体信息] ○ 条目 %s 探测失败: %s", id, failureDetail)
			if serviceFailure {
				consecutiveServiceFailures++
				if consecutiveServiceFailures >= 3 {
					log.Printf("[Emby媒体信息] 连续服务故障 3 项，暂停本轮以免持续请求远端")
					return
				}
				if !waitEmbyProbe(ctx, embyProbeRetryDelay) {
					return
				}
			} else {
				consecutiveServiceFailures = 0
			}
		}
		if !waitEmbyProbe(ctx, embyProbePace) {
			return
		}
	}
}

func StartEmbyMediaProbeWorker(h *Handler) {
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			<-stopCh
			cancel()
		}()
		for ctx.Err() == nil {
			if h.embyProbeEnabled() {
				h.runEmbyMediaProbe(ctx)
				if !waitEmbyProbe(ctx, embyProbeScanDelay) {
					return
				}
			} else if !waitEmbyProbe(ctx, 15*time.Second) {
				return
			}
		}
	}()
}
