package api

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golift.io/udf"
	"gorm.io/gorm"

	"strmhub/internal/config"
	"strmhub/internal/model"
)

var isoMediaPickCodeRe = regexp.MustCompile(`^[A-Za-z0-9]{8,64}$`)

// A lease exists only while the persistent worker asks local Emby to probe
// this exact source. Infuse and other player requests always keep ISO /d/ 302.
type isoProbeLease struct {
	file     model.SyncedFile
	expires  time.Time
	reason   string
	requests int
}

var isoProbeLeases = struct {
	sync.Mutex
	active map[string]*isoProbeLease
}{active: make(map[string]*isoProbeLease)}

func beginISOMediaProbe(db *gorm.DB, cfg *config.Config, strmPath string) (*isoProbeLease, error) {
	if !strings.HasSuffix(strings.ToLower(strmPath), ".iso.strm") {
		return nil, nil
	}
	directURL := readStrmDirectURL(db, cfg, strmPath)
	pickCode := pickcodeOfDirectURL(directURL)
	if i := strings.LastIndex(pickCode, "."); i > 0 {
		pickCode = pickCode[:i]
	}
	if !isoMediaPickCodeRe.MatchString(pickCode) {
		return nil, errors.New("ISO STRM 未指向有效的 115 /d/ 直链")
	}
	var file model.SyncedFile
	if db == nil || db.Where("pick_code = ? AND kind = ?", pickCode, "video").First(&file).Error != nil ||
		!strings.HasSuffix(strings.ToLower(file.RelPath), ".iso.strm") {
		return nil, errors.New("ISO 未命中同步台账")
	}
	source, err := getISOMediaProbeSource(db, cfg, file)
	if err != nil {
		if errors.Is(err, errISOUnsupported) {
			return nil, err
		}
		return nil, errors.New("ISO 媒体信息读取失败，等待重试")
	}
	file.Size = source.isoSize
	lease := &isoProbeLease{file: file, expires: time.Now().Add(3 * time.Minute)}
	isoProbeLeases.Lock()
	isoProbeLeases.active[pickCode] = lease
	isoProbeLeases.Unlock()
	return lease, nil
}

func finishISOMediaProbe(lease *isoProbeLease) string {
	if lease == nil {
		return ""
	}
	isoProbeLeases.Lock()
	defer isoProbeLeases.Unlock()
	if isoProbeLeases.active[lease.file.PickCode] == lease {
		delete(isoProbeLeases.active, lease.file.PickCode)
	}
	return lease.reason
}

func isoMetadataRequestLease(request *http.Request, pickCode string) *isoProbeLease {
	// RemoteAddr is the socket peer; forwarded headers cannot grant a lease.
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil || !net.ParseIP(host).IsLoopback() || !strings.HasPrefix(request.UserAgent(), "Lavf/") {
		return nil
	}
	isoProbeLeases.Lock()
	defer isoProbeLeases.Unlock()
	lease := isoProbeLeases.active[pickCode]
	if lease == nil || time.Now().After(lease.expires) {
		return nil
	}
	return lease
}

type isoMediaSource struct {
	isoSize     int64
	main        udf.File
	fingerprint string
	createdAt   time.Time
}

var isoMediaCache = struct {
	sync.Mutex
	sources map[string]*isoMediaSource
}{sources: make(map[string]*isoMediaSource)}

func serveISOMetadataIfLeased(c *gin.Context, db *gorm.DB, cfg *config.Config, pickCode string) bool {
	lease := isoMetadataRequestLease(c.Request, pickCode)
	if lease == nil {
		return false
	}
	file := lease.file
	isoProbeLeases.Lock()
	lease.requests++
	isoProbeLeases.Unlock()
	source, err := getISOMediaProbeSource(db, cfg, file)
	if err != nil {
		// Never expose signed links from errors produced by the UDF reader.
		if errors.Is(err, errISOUnsupported) {
			log.Printf("[ISO媒体信息] 文件 %s 无法自动识别完整主片: %v", file.FileID, err)
			c.String(http.StatusUnprocessableEntity, "%s", err.Error())
		} else {
			log.Printf("[ISO媒体信息] 文件 %s 远端读取失败，等待重试", file.FileID)
			c.String(http.StatusBadGateway, "ISO remote read failed; retry later")
		}
		isoProbeLeases.Lock()
		if errors.Is(err, errISOUnsupported) {
			lease.reason = err.Error()
		} else {
			lease.reason = "ISO 远端读取失败，等待重试"
		}
		isoProbeLeases.Unlock()
		return true
	}
	reader, err := source.main.NewReader()
	if err != nil {
		c.Status(http.StatusBadGateway)
		return true
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Type", "video/mp2t")
	http.ServeContent(c.Writer, c.Request, "main.m2ts", time.Time{}, reader)
	return true
}

func getISOMediaProbeSource(db *gorm.DB, cfg *config.Config, file model.SyncedFile) (*isoMediaSource, error) {
	isoMediaCache.Lock()
	defer isoMediaCache.Unlock()
	fingerprint := fmt.Sprintf("%s:%s:%d", file.FileID, file.Sha1, file.Size)
	if source := isoMediaCache.sources[file.PickCode]; source != nil && source.fingerprint == fingerprint {
		return source, nil
	}
	reader := &isoRangeReader{
		db: db, cfg: cfg, pickCode: file.PickCode, size: file.Size,
		blockSize: 64 << 10, blocks: make(map[int64][]byte),
		client: &http.Client{Timeout: 30 * time.Second},
	}
	// Precise incremental events may omit the size. A one-byte Range request
	// discovers it without broadening the sync or downloading the image.
	if reader.size <= 0 {
		if err := reader.discoverSize(); err != nil {
			return nil, err
		}
	}
	disc, err := udf.NewUdfFromReader(reader)
	if err != nil {
		return nil, err
	}
	root, err := disc.ReadDir(nil)
	if err != nil {
		return nil, err
	}
	bdmv := isoFindDir(root, "BDMV")
	if bdmv == nil {
		return nil, isoUnsupported("BDMV directory missing; DVD ISO is not supported")
	}
	entries, err := bdmv.ReadDir()
	if err != nil {
		return nil, err
	}
	stream := isoFindDir(entries, "STREAM")
	if stream == nil {
		return nil, isoUnsupported("BDMV/STREAM directory missing")
	}
	files, err := stream.ReadDir()
	if err != nil {
		return nil, err
	}
	var clips []udf.File
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(strings.ToLower(f.Name()), ".m2ts") {
			clips = append(clips, f)
		}
	}
	sort.Slice(clips, func(i, j int) bool { return clips[i].Size() > clips[j].Size() })
	if len(clips) == 0 || clips[0].Size() < reader.size/2 ||
		(len(clips) > 1 && clips[0].Size() < 2*clips[1].Size()) {
		return nil, isoUnsupported("no dominant main clip; multi-clip or multi-title disc")
	}
	if err := verifyISOMainPlaylist(entries, clips[0].Name()); err != nil {
		return nil, err
	}
	// Directory reads need small ranges; ffprobe benefits from larger ranges.
	reader.setBlockSize(4 << 20)
	source := &isoMediaSource{isoSize: reader.size, main: clips[0], fingerprint: fingerprint, createdAt: time.Now()}
	// Each retained source holds a media block cache. Keep idle memory bounded
	// as the worker encounters more discs; active readers remain valid.
	if len(isoMediaCache.sources) >= 4 {
		oldestKey := ""
		var oldestTime time.Time
		for key, cached := range isoMediaCache.sources {
			if oldestKey == "" || cached.createdAt.Before(oldestTime) {
				oldestKey, oldestTime = key, cached.createdAt
			}
		}
		delete(isoMediaCache.sources, oldestKey)
	}
	isoMediaCache.sources[file.PickCode] = source
	return source, nil
}

func isoFindDir(entries []udf.File, name string) *udf.File {
	for i := range entries {
		if entries[i].IsDir() && strings.EqualFold(entries[i].Name(), name) {
			return &entries[i]
		}
	}
	return nil
}

// isoRangeReader exposes a signed 115 CDN link as a bounded-memory ReaderAt.
// It refreshes the link on expiry or rejection, including after IP changes.
type isoRangeReader struct {
	sync.Mutex
	db        *gorm.DB
	cfg       *config.Config
	pickCode  string
	size      int64
	blockSize int64
	blocks    map[int64][]byte
	order     []int64
	url       string
	headers   map[string]string
	expires   time.Time
	client    *http.Client
	resolve   func() (string, map[string]string, error)
}

func (r *isoRangeReader) setBlockSize(size int64) {
	r.Lock()
	defer r.Unlock()
	r.blockSize = size
	r.blocks = make(map[int64][]byte)
	r.order = nil
}

func (r *isoRangeReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if off >= r.size {
		return 0, io.EOF
	}
	r.Lock()
	defer r.Unlock()
	done := 0
	for done < len(p) && off+int64(done) < r.size {
		pos := off + int64(done)
		index := pos / r.blockSize
		block, ok := r.blocks[index]
		if !ok {
			var err error
			block, err = r.fetchBlock(index)
			if err != nil {
				return done, err
			}
			r.blocks[index] = block
			r.order = append(r.order, index)
			if len(r.order) > 4 {
				delete(r.blocks, r.order[0])
				r.order = r.order[1:]
			}
		}
		n := copy(p[done:], block[pos%r.blockSize:])
		if n == 0 {
			return done, io.ErrUnexpectedEOF
		}
		done += n
	}
	if done < len(p) {
		return done, io.EOF
	}
	return done, nil
}

func (r *isoRangeReader) refreshLink() error {
	resolve := r.resolve
	if resolve == nil {
		resolve = func() (string, map[string]string, error) {
			return proxyDownloadURLFull(r.db, r.cfg, r.pickCode, ua115Download)
		}
	}
	link, headers, err := resolve()
	if err != nil || link == "" {
		return errors.New("115 download link unavailable")
	}
	r.url, r.headers, r.expires = link, headers, time.Now().Add(25*time.Minute)
	return nil
}

func (r *isoRangeReader) discoverSize() error {
	if r.url == "" || time.Now().After(r.expires) {
		if err := r.refreshLink(); err != nil {
			return err
		}
	}
	req, err := http.NewRequest(http.MethodGet, r.url, nil)
	if err != nil {
		return errors.New("invalid 115 download link")
	}
	for key, value := range r.headers {
		req.Header.Set(key, value)
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := r.client.Do(req)
	if err != nil {
		return errors.New("ISO size probe failed")
	}
	defer resp.Body.Close()
	value := strings.TrimPrefix(resp.Header.Get("Content-Range"), "bytes 0-0/")
	size, err := strconv.ParseInt(value, 10, 64)
	if resp.StatusCode != http.StatusPartialContent || err != nil || size <= 0 {
		return errors.New("ISO size unavailable from range response")
	}
	r.size = size
	return nil
}

func (r *isoRangeReader) fetchBlock(index int64) ([]byte, error) {
	start := index * r.blockSize
	end := min(start+r.blockSize, r.size) - 1
	for attempt := 0; attempt < 2; attempt++ {
		if r.url == "" || time.Now().After(r.expires) {
			if err := r.refreshLink(); err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequest(http.MethodGet, r.url, nil)
		if err != nil {
			return nil, errors.New("invalid 115 download link")
		}
		for k, v := range r.headers {
			req.Header.Set(k, v)
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		resp, err := r.client.Do(req)
		if err != nil {
			r.url = ""
			if attempt == 0 {
				continue
			}
			return nil, errors.New("115 range request failed")
		}
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			r.url = ""
			if attempt == 0 {
				continue
			}
			return nil, errors.New("115 range link rejected")
		}
		if resp.StatusCode != http.StatusPartialContent ||
			!isoContentRangeMatches(resp.Header.Get("Content-Range"), start, end, r.size) {
			resp.Body.Close()
			return nil, fmt.Errorf("invalid 115 range response: HTTP %d", resp.StatusCode)
		}
		want := end - start + 1
		data, err := io.ReadAll(io.LimitReader(resp.Body, want+1))
		resp.Body.Close()
		if err != nil || int64(len(data)) != want {
			return nil, errors.New("short 115 range response")
		}
		return data, nil
	}
	return nil, errors.New("115 range request failed")
}

func isoContentRangeMatches(value string, start, end, size int64) bool {
	if !strings.HasPrefix(value, "bytes ") {
		return false
	}
	parts := strings.SplitN(strings.TrimPrefix(value, "bytes "), "/", 2)
	if len(parts) != 2 {
		return false
	}
	total, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || total != size {
		return false
	}
	bounds := strings.SplitN(parts[0], "-", 2)
	if len(bounds) != 2 {
		return false
	}
	gotStart, errStart := strconv.ParseInt(bounds[0], 10, 64)
	gotEnd, errEnd := strconv.ParseInt(bounds[1], 10, 64)
	return errStart == nil && errEnd == nil && gotStart == start && gotEnd == end
}

func registerISOMetadataRoutes(r gin.IRoutes, db *gorm.DB, cfg *config.Config) {
	metadata := func(c *gin.Context) {
		if !serveISOMetadataIfLeased(c, db, cfg, c.Param("pickcode")) {
			c.Status(http.StatusNotFound)
		}
	}
	r.GET("/iso-media/:pickcode/main.m2ts", metadata)
	r.HEAD("/iso-media/:pickcode/main.m2ts", metadata)
}
