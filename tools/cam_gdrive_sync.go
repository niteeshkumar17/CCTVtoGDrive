package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	ClientID        string `json:"client_id"`
	ClientSecret    string `json:"client_secret"`
	RefreshToken    string `json:"refresh_token"`
	FolderID        string `json:"folder_id"`
	FolderName      string `json:"folder_name"`
	RetentionHrs    int    `json:"retention_hours"`
	TZOffsetMinutes *int   `json:"tz_offset_minutes,omitempty"`
	MediaDir        string `json:"media_dir"`
	StateFile       string `json:"state_file"`
	CACertFile      string `json:"ca_cert_file"`
	PollSeconds     int    `json:"poll_seconds"`
}

type TokenResp struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

type DriveFile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	MimeType    string `json:"mimeType"`
	CreatedTime string `json:"createdTime"`
}

type DriveListResp struct {
	NextPageToken string      `json:"nextPageToken"`
	Files         []DriveFile `json:"files"`
}

const (
	scottyChunkSize = 512 * 1024 // 524,288 bytes (2 * 256 KiB, required by Google Scotty)
	maxMoofSize     = 1 * 1024 * 1024
	maxLogBytes     = 32 * 1024
	keepLogBytes    = 16 * 1024
)

var (
	driveAPIBase  = "https://www.googleapis.com"
	oauthTokenURL = "https://oauth2.googleapis.com/token"
	sleepFunc     = time.Sleep

	httpClient  *http.Client
	cachedToken string
	tokenExpiry time.Time
	folderCache = make(map[string]string)
	scottyBuf   = make([]byte, scottyChunkSize)

	logMu       sync.Mutex
	logFilePath string
)

func logf(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}

	logMu.Lock()
	defer logMu.Unlock()

	if logFilePath == "" {
		fmt.Print(msg)
		return
	}

	f, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Print(msg)
		return
	}
	_, _ = f.WriteString(msg)
	f.Close()

	if st, err := os.Stat(logFilePath); err == nil && st.Size() > maxLogBytes {
		if b, err := os.ReadFile(logFilePath); err == nil && len(b) > keepLogBytes {
			tail := b[len(b)-keepLogBytes:]
			if idx := bytes.IndexByte(tail, '\n'); idx >= 0 && idx+1 < len(tail) {
				tail = tail[idx+1:]
			}
			tmp := logFilePath + ".tmp"
			if os.WriteFile(tmp, tail, 0644) == nil {
				_ = os.Rename(tmp, logFilePath)
			}
		}
	}
}

func tzOffsetMinutes(cfg *Config) int {
	if cfg == nil || cfg.TZOffsetMinutes == nil {
		return 330
	}
	return *cfg.TZOffsetMinutes
}

func cameraNow(tzOffsetMin int) time.Time {
	return time.Now().UTC().Add(time.Duration(tzOffsetMin) * time.Minute)
}

func validateConfig(cfg *Config) error {
	if strings.TrimSpace(cfg.ClientID) == "" {
		return fmt.Errorf("missing client_id")
	}
	if strings.TrimSpace(cfg.ClientSecret) == "" {
		return fmt.Errorf("missing client_secret")
	}
	if strings.TrimSpace(cfg.RefreshToken) == "" {
		return fmt.Errorf("missing refresh_token")
	}
	if strings.TrimSpace(cfg.FolderID) == "" && strings.TrimSpace(cfg.FolderName) == "" {
		return fmt.Errorf("missing folder_id or folder_name")
	}
	return nil
}

func dropPageCache() {
	_ = os.WriteFile("/proc/sys/vm/drop_caches", []byte("1\n"), 0644)
}

type cacheDroppingReader struct {
	r     io.Reader
	bytes int64
}

func (c *cacheDroppingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.bytes += int64(n)
		if c.bytes >= 2*1024*1024 {
			c.bytes = 0
			dropPageCache()
		}
	}
	return n, err
}

func initHTTP(caFile string) error {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pemData, err := os.ReadFile(caFile)
		if err != nil {
			return fmt.Errorf("read CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemData) {
			return fmt.Errorf("failed to parse CA bundle")
		}
		tlsCfg.RootCAs = pool
	}
	httpClient = &http.Client{
		Timeout: 120 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:       tlsCfg,
			MaxIdleConns:          4,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   20 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			WriteBufferSize:       32 * 1024,
			ReadBufferSize:        32 * 1024,
		},
	}
	return nil
}

func setFolderCache(key, id string) {
	if len(folderCache) >= 200 {
		folderCache = make(map[string]string)
	}
	folderCache[key] = id
}

func invalidateToken() {
	cachedToken = ""
	tokenExpiry = time.Time{}
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

func escapeDriveQueryValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `'`, `\'`)
}

func isRetryableHTTPStatus(code int, body []byte) bool {
	if code == 429 || (code >= 500 && code <= 599) {
		return true
	}
	if code == 403 {
		return !bytes.Contains(body, []byte("storageQuotaExceeded"))
	}
	return false
}

func backoffDelay(try int, retryAfter string) time.Duration {
	if retryAfter != "" {
		if sec, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && sec > 0 {
			d := time.Duration(sec) * time.Second
			if d > 30*time.Second {
				d = 30 * time.Second
			}
			return d
		}
	}
	shift := try
	if shift > 5 {
		shift = 5
	}
	d := (500 * time.Millisecond) << shift
	if d > 15*time.Second {
		d = 15 * time.Second
	}
	return d
}

func getAccessToken(cfg *Config) (string, error) {
	if cachedToken != "" && time.Now().Before(tokenExpiry) {
		return cachedToken, nil
	}
	vals := url.Values{
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
		"refresh_token": {cfg.RefreshToken},
		"grant_type":    {"refresh_token"},
	}
	resp, err := httpClient.PostForm(oauthTokenURL, vals)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("token HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	var tr TokenResp
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", err
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("empty access_token in response")
	}
	cachedToken = tr.AccessToken
	exp := tr.ExpiresIn
	if exp <= 120 {
		exp = 3000
	}
	tokenExpiry = time.Now().Add(time.Duration(exp-120) * time.Second)
	return cachedToken, nil
}

func ensureSubfolder(cfg *Config, parentID, name string) (string, error) {
	cacheKey := parentID + "/" + name
	if id, ok := folderCache[cacheKey]; ok && id != "" {
		return id, nil
	}
	q := fmt.Sprintf("mimeType = 'application/vnd.google-apps.folder' and name = '%s' and trashed = false", escapeDriveQueryValue(name))
	if parentID != "" {
		q += fmt.Sprintf(" and '%s' in parents", escapeDriveQueryValue(parentID))
	}
	u := driveAPIBase + "/drive/v3/files?pageSize=1&fields=files(id,name)&q=" + url.QueryEscape(q)
	gotEmptyList := false
	refreshed := false
	var lastErr error
	for try := 0; try < 20; try++ {
		token, err := getAccessToken(cfg)
		if err != nil {
			return "", err
		}
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			sleepFunc(backoffDelay(try, ""))
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		resp.Body.Close()
		if resp.StatusCode == 200 {
			var list DriveListResp
			if err := json.Unmarshal(body, &list); err != nil {
				return "", fmt.Errorf("ensureSubfolder(%s) decode: %w", name, err)
			}
			if len(list.Files) > 0 {
				setFolderCache(cacheKey, list.Files[0].ID)
				return list.Files[0].ID, nil
			}
			gotEmptyList = true
			break
		}
		if resp.StatusCode == 401 && !refreshed {
			invalidateToken()
			refreshed = true
			continue
		}
		lastErr = fmt.Errorf("ensureSubfolder(%s) GET HTTP %d: %s", name, resp.StatusCode, snippet(body))
		if !isRetryableHTTPStatus(resp.StatusCode, body) {
			return "", lastErr
		}
		sleepFunc(backoffDelay(try, resp.Header.Get("Retry-After")))
	}
	if !gotEmptyList {
		if lastErr != nil {
			return "", lastErr
		}
		return "", fmt.Errorf("ensureSubfolder(%s) GET retries exhausted", name)
	}

	meta := map[string]interface{}{
		"name":     name,
		"mimeType": "application/vnd.google-apps.folder",
	}
	if parentID != "" {
		meta["parents"] = []string{parentID}
	}
	mb, _ := json.Marshal(meta)
	refreshed = false
	lastErr = nil
	for try := 0; try < 15; try++ {
		token, err := getAccessToken(cfg)
		if err != nil {
			return "", err
		}
		creq, err := http.NewRequest("POST", driveAPIBase+"/drive/v3/files", bytes.NewReader(mb))
		if err != nil {
			return "", err
		}
		creq.Header.Set("Authorization", "Bearer "+token)
		creq.Header.Set("Content-Type", "application/json")
		cresp, err := httpClient.Do(creq)
		if err != nil {
			lastErr = err
			sleepFunc(backoffDelay(try, ""))
			continue
		}
		cbody, _ := io.ReadAll(io.LimitReader(cresp.Body, 64*1024))
		cresp.Body.Close()
		if cresp.StatusCode == 200 || cresp.StatusCode == 201 {
			var df DriveFile
			if err := json.Unmarshal(cbody, &df); err != nil {
				return "", err
			}
			setFolderCache(cacheKey, df.ID)
			return df.ID, nil
		}
		if cresp.StatusCode == 401 && !refreshed {
			invalidateToken()
			refreshed = true
			continue
		}
		lastErr = fmt.Errorf("ensureSubfolder(%s) POST HTTP %d: %s", name, cresp.StatusCode, snippet(cbody))
		if !isRetryableHTTPStatus(cresp.StatusCode, cbody) {
			return "", lastErr
		}
		sleepFunc(backoffDelay(try, cresp.Header.Get("Retry-After")))
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("ensureSubfolder(%s) POST retries exhausted", name)
}

func deleteDriveObject(cfg *Config, fileID string) error {
	if fileID == "" {
		return nil
	}
	refreshed := false
	var lastErr error
	for try := 0; try < 15; try++ {
		token, err := getAccessToken(cfg)
		if err != nil {
			return err
		}
		req, err := http.NewRequest("DELETE", driveAPIBase+"/drive/v3/files/"+fileID, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			sleepFunc(backoffDelay(try, ""))
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		if resp.StatusCode == 204 || resp.StatusCode == 200 || resp.StatusCode == 404 {
			for k, v := range folderCache {
				if v == fileID {
					delete(folderCache, k)
				}
			}
			return nil
		}
		if resp.StatusCode == 401 && !refreshed {
			invalidateToken()
			refreshed = true
			continue
		}
		lastErr = fmt.Errorf("DELETE %s HTTP %d: %s", fileID, resp.StatusCode, snippet(body))
		if !isRetryableHTTPStatus(resp.StatusCode, body) {
			return lastErr
		}
		sleepFunc(backoffDelay(try, resp.Header.Get("Retry-After")))
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("DELETE %s retries exhausted", fileID)
}

func driveListFiles(cfg *Config, query, fields string) ([]DriveFile, error) {
	var all []DriveFile
	pageToken := ""
	fullFields := "nextPageToken," + fields
	for {
		u := driveAPIBase + "/drive/v3/files?pageSize=100&fields=" + url.QueryEscape(fullFields) + "&q=" + url.QueryEscape(query)
		if pageToken != "" {
			u += "&pageToken=" + url.QueryEscape(pageToken)
		}
		var pageResp DriveListResp
		ok := false
		refreshed := false
		var lastErr error
		for try := 0; try < 15; try++ {
			token, err := getAccessToken(cfg)
			if err != nil {
				return nil, err
			}
			req, err := http.NewRequest("GET", u, nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := httpClient.Do(req)
			if err != nil {
				lastErr = err
				sleepFunc(backoffDelay(try, ""))
				continue
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
			resp.Body.Close()
			if resp.StatusCode == 200 {
				if err := json.Unmarshal(body, &pageResp); err != nil {
					return nil, fmt.Errorf("driveListFiles decode: %w", err)
				}
				ok = true
				break
			}
			if resp.StatusCode == 401 && !refreshed {
				invalidateToken()
				refreshed = true
				continue
			}
			lastErr = fmt.Errorf("driveListFiles HTTP %d: %s", resp.StatusCode, snippet(body))
			if !isRetryableHTTPStatus(resp.StatusCode, body) {
				return nil, lastErr
			}
			sleepFunc(backoffDelay(try, resp.Header.Get("Retry-After")))
		}
		if !ok {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, fmt.Errorf("driveListFiles retries exhausted")
		}
		all = append(all, pageResp.Files...)
		if pageResp.NextPageToken == "" {
			break
		}
		pageToken = pageResp.NextPageToken
	}
	return all, nil
}

func driveFileExists(cfg *Config, folderID, name string) (bool, error) {
	q := fmt.Sprintf("name = '%s' and trashed = false", escapeDriveQueryValue(name))
	if folderID != "" {
		q += fmt.Sprintf(" and '%s' in parents", escapeDriveQueryValue(folderID))
	}
	files, err := driveListFiles(cfg, q, "files(id,name)")
	if err != nil {
		return false, err
	}
	return len(files) > 0, nil
}

func initResumableUpload(cfg *Config, folderID, remoteName string, totalSize int64) (string, error) {
	meta := map[string]interface{}{
		"name": remoteName,
	}
	if folderID != "" {
		meta["parents"] = []string{folderID}
	}
	metaBytes, _ := json.Marshal(meta)

	refreshed := false
	var lastErr error
	for try := 0; try < 25; try++ {
		token, err := getAccessToken(cfg)
		if err != nil {
			return "", err
		}
		initReq, err := http.NewRequest("POST", driveAPIBase+"/upload/drive/v3/files?uploadType=resumable", bytes.NewReader(metaBytes))
		if err != nil {
			return "", err
		}
		initReq.Header.Set("Authorization", "Bearer "+token)
		initReq.Header.Set("Content-Type", "application/json; charset=UTF-8")
		initReq.Header.Set("X-Upload-Content-Type", "video/mp4")
		if totalSize > 0 {
			initReq.Header.Set("X-Upload-Content-Length", fmt.Sprintf("%d", totalSize))
		}

		initResp, err := httpClient.Do(initReq)
		if err != nil {
			lastErr = fmt.Errorf("init resumable: %w", err)
			sleepFunc(backoffDelay(try, ""))
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(initResp.Body, 8192))
		initResp.Body.Close()
		if initResp.StatusCode == 200 {
			loc := initResp.Header.Get("Location")
			if loc != "" {
				return loc, nil
			}
			lastErr = fmt.Errorf("init resumable: HTTP 200 missing Location header")
		} else if initResp.StatusCode == 401 && !refreshed {
			invalidateToken()
			refreshed = true
			continue
		} else {
			if initResp.StatusCode == 404 {
				folderCache = make(map[string]string)
			}
			lastErr = fmt.Errorf("init resumable HTTP %d: %s", initResp.StatusCode, snippet(body))
			if !isRetryableHTTPStatus(initResp.StatusCode, body) {
				return "", lastErr
			}
		}
		sleepFunc(backoffDelay(try, initResp.Header.Get("Retry-After")))
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("failed to get resumable upload URL after retries")
}

// uploadStreamResumable streams any io.Reader to uploadURL in 512 KB Scotty chunks (308 Resume Incomplete)
// using a 1-byte lookahead, and retries the final chunk from RAM up to 40 times on retryable errors.
func uploadStreamResumable(uploadURL string, src io.Reader) (string, error) {
	var offset int64 = 0
	var peek [1]byte
	hasPeek := false

	for {
		startIdx := 0
		if hasPeek {
			scottyBuf[0] = peek[0]
			startIdx = 1
			hasPeek = false
		}
		n, err := io.ReadFull(src, scottyBuf[startIdx:])
		filled := startIdx + n
		if filled == 0 {
			return "", fmt.Errorf("empty stream")
		}

		isLast := false
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			isLast = true
		} else if err != nil {
			return "", err
		} else {
			// scottyBuf is completely full (512 KB); peek 1 byte to check if more data follows
			pn, perr := io.ReadFull(src, peek[:])
			if pn == 1 {
				hasPeek = true
			} else if perr == io.EOF || perr == io.ErrUnexpectedEOF {
				isLast = true
			} else if perr != nil {
				return "", perr
			}
		}

		chunk := scottyBuf[:filled]
		if !isLast {
			contentRange := fmt.Sprintf("bytes %d-%d/*", offset, offset+int64(filled)-1)
			ok := false
			var lastErr error
			for try := 0; try < 15; try++ {
				req, err := http.NewRequest("PUT", uploadURL, bytes.NewReader(chunk))
				if err != nil {
					return "", err
				}
				req.ContentLength = int64(filled)
				req.Header.Set("Content-Type", "video/mp4")
				req.Header.Set("Content-Range", contentRange)
				resp, err := httpClient.Do(req)
				if err != nil {
					lastErr = err
					sleepFunc(backoffDelay(try, ""))
					continue
				}
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
				resp.Body.Close()
				if resp.StatusCode == 308 || resp.StatusCode == 200 || resp.StatusCode == 201 {
					ok = true
					break
				}
				lastErr = fmt.Errorf("intermediate chunk offset %d HTTP %d: %s", offset, resp.StatusCode, snippet(body))
				if !isRetryableHTTPStatus(resp.StatusCode, body) {
					return "", lastErr
				}
				sleepFunc(backoffDelay(try, resp.Header.Get("Retry-After")))
			}
			if !ok {
				if lastErr != nil {
					return "", lastErr
				}
				return "", fmt.Errorf("intermediate chunk failed at offset %d", offset)
			}
			offset += int64(filled)
			dropPageCache()
		} else {
			totalSize := offset + int64(filled)
			contentRange := fmt.Sprintf("bytes %d-%d/%d", offset, totalSize-1, totalSize)
			queryRange := fmt.Sprintf("bytes */%d", totalSize)
			sendZeroByte := false
			var lastErr error
			for try := 0; try < 40; try++ {
				var req *http.Request
				if sendZeroByte {
					req, err = http.NewRequest("PUT", uploadURL, nil)
					if err != nil {
						return "", err
					}
					req.ContentLength = 0
					req.Header.Set("Content-Range", queryRange)
				} else {
					req, err = http.NewRequest("PUT", uploadURL, bytes.NewReader(chunk))
					if err != nil {
						return "", err
					}
					req.ContentLength = int64(filled)
					req.Header.Set("Content-Type", "video/mp4")
					req.Header.Set("Content-Range", contentRange)
				}
				resp, err := httpClient.Do(req)
				if err != nil {
					lastErr = err
					sendZeroByte = true
					d := backoffDelay(try, "")
					if d > 15*time.Second {
						d = 15 * time.Second
					}
					sleepFunc(d)
					continue
				}
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
				resp.Body.Close()
				if resp.StatusCode == 200 || resp.StatusCode == 201 {
					var df DriveFile
					_ = json.Unmarshal(body, &df)
					return df.ID, nil
				}
				if resp.StatusCode == 308 {
					sendZeroByte = false
				} else {
					lastErr = fmt.Errorf("final commit HTTP %d: %s", resp.StatusCode, snippet(body))
					if !isRetryableHTTPStatus(resp.StatusCode, body) {
						return "", lastErr
					}
					// Probe with a 0-byte query only after 5xx/network errors; keep retrying full chunk on 403/429 rate-limits.
					sendZeroByte = resp.StatusCode >= 500 && resp.StatusCode <= 599
				}
				d := backoffDelay(try, resp.Header.Get("Retry-After"))
				if d > 15*time.Second {
					d = 15 * time.Second
				}
				sleepFunc(d)
			}
			if lastErr != nil {
				return "", fmt.Errorf("final commit retries exhausted: %w", lastErr)
			}
			return "", fmt.Errorf("final commit chunk rate-limited after retries")
		}
	}
}

func uploadSingleFile(cfg *Config, folderID, localPath, remoteName string) (string, error) {
	dropPageCache()
	defer dropPageCache()

	st, err := os.Stat(localPath)
	if err != nil {
		return "", err
	}
	uploadURL, err := initResumableUpload(cfg, folderID, remoteName, st.Size())
	if err != nil {
		return "", err
	}
	f, err := os.Open(localPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	return uploadStreamResumable(uploadURL, f)
}

func patchMoof(b []byte, seqNum uint32, timeOffsetMS uint64) uint64 {
	pos := 8
	end := len(b)
	var maxEndDT uint64
	for pos+8 <= end {
		csz := int(binary.BigEndian.Uint32(b[pos : pos+4]))
		ctyp := string(b[pos+4 : pos+8])
		if csz < 8 || pos+csz > end {
			break
		}
		if ctyp == "mfhd" && csz >= 16 {
			binary.BigEndian.PutUint32(b[pos+12:pos+16], seqNum)
		} else if ctyp == "traf" {
			tsub := pos + 8
			tend := pos + csz
			var tfdtVal uint64
			var durSum uint64
			var defaultDur uint32
			for tsub+8 <= tend {
				tsz := int(binary.BigEndian.Uint32(b[tsub : tsub+4]))
				ttyp := string(b[tsub+4 : tsub+8])
				if tsz < 8 || tsub+tsz > tend {
					break
				}
				if ttyp == "tfhd" && tsz >= 16 {
					vf := binary.BigEndian.Uint32(b[tsub+8 : tsub+12])
					off := tsub + 16
					if vf&0x1 != 0 {
						off += 8
					}
					if vf&0x2 != 0 {
						off += 4
					}
					if vf&0x8 != 0 && off+4 <= tsub+tsz {
						defaultDur = binary.BigEndian.Uint32(b[off : off+4])
					}
				} else if ttyp == "tfdt" && tsz >= 16 {
					ver := b[tsub+8]
					if ver == 1 && tsz >= 20 {
						dt := binary.BigEndian.Uint64(b[tsub+12 : tsub+20])
						tfdtVal = dt + timeOffsetMS
						binary.BigEndian.PutUint64(b[tsub+12:tsub+20], tfdtVal)
					} else {
						dt := uint64(binary.BigEndian.Uint32(b[tsub+12 : tsub+16]))
						tfdtVal = dt + timeOffsetMS
						binary.BigEndian.PutUint32(b[tsub+12:tsub+16], uint32(tfdtVal))
					}
				} else if ttyp == "trun" && tsz >= 16 {
					vf := binary.BigEndian.Uint32(b[tsub+8 : tsub+12])
					sc := int(binary.BigEndian.Uint32(b[tsub+12 : tsub+16]))
					off := tsub + 16
					if vf&0x1 != 0 {
						off += 4
					}
					if vf&0x4 != 0 {
						off += 4
					}
					for i := 0; i < sc && off <= tsub+tsz; i++ {
						if vf&0x100 != 0 {
							if off+4 <= tsub+tsz {
								durSum += uint64(binary.BigEndian.Uint32(b[off : off+4]))
							}
							off += 4
						} else {
							durSum += uint64(defaultDur)
						}
						if vf&0x200 != 0 {
							off += 4
						}
						if vf&0x400 != 0 {
							off += 4
						}
						if vf&0x800 != 0 {
							off += 4
						}
					}
				}
				tsub += tsz
			}
			if tfdtVal+durSum > maxEndDT {
				maxEndDT = tfdtVal + durSum
			}
		}
		pos += csz
	}
	return maxEndDT
}

type writeTracker struct {
	w   io.Writer
	err error
}

func (wt *writeTracker) Write(p []byte) (int, error) {
	n, err := wt.w.Write(p)
	if err != nil {
		wt.err = err
	}
	return n, err
}

func padZeroes(wt *writeTracker, count int64, buf []byte) error {
	for i := range buf {
		buf[i] = 0
	}
	for count > 0 {
		n := int64(len(buf))
		if count < n {
			n = count
		}
		if _, err := wt.Write(buf[:n]); err != nil {
			return err
		}
		count -= n
	}
	return nil
}

func mergeOneClip(w io.Writer, path string, copyBuf []byte, moofBuf *[]byte, wroteInit *bool, seqNum *uint32, offsetMS uint64) (uint64, error) {
	dropPageCache()
	f, err := os.Open(path)
	if err != nil {
		return offsetMS, nil
	}
	defer func() {
		f.Close()
		dropPageCache()
	}()

	fileSize := int64(-1)
	if st, err := f.Stat(); err == nil {
		fileSize = st.Size()
	}

	br := bufio.NewReaderSize(&cacheDroppingReader{r: f}, 32*1024)
	wt := &writeTracker{w: w}
	fileMaxEnd := offsetMS
	var filePos int64
	var hdr [8]byte

	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return fileMaxEnd, nil
		}
		filePos += 8
		sz := int64(binary.BigEndian.Uint32(hdr[0:4]))
		typ := string(hdr[4:8])
		if sz < 8 || sz > 32*1024*1024 {
			return fileMaxEnd, nil
		}
		payloadLen := sz - 8

		switch typ {
		case "ftyp", "free", "moov":
			if !*wroteInit {
				if fileSize >= 0 && filePos+payloadLen > fileSize {
					return fileMaxEnd, nil
				}
				if _, err := wt.Write(hdr[:]); err != nil {
					return fileMaxEnd, err
				}
				n, err := io.CopyBuffer(wt, io.LimitReader(br, payloadLen), copyBuf)
				filePos += n
				if wt.err != nil {
					return fileMaxEnd, wt.err
				}
				if n < payloadLen {
					if perr := padZeroes(wt, payloadLen-n, copyBuf); perr != nil {
						return fileMaxEnd, perr
					}
					return fileMaxEnd, nil
				}
				if err != nil {
					return fileMaxEnd, nil
				}
				if typ == "moov" {
					*wroteInit = true
				}
			} else {
				n, err := io.CopyBuffer(io.Discard, io.LimitReader(br, payloadLen), copyBuf)
				filePos += n
				if err != nil || n < payloadLen {
					return fileMaxEnd, nil
				}
			}
		case "moof":
			if sz > maxMoofSize {
				return fileMaxEnd, nil
			}
			if cap(*moofBuf) < int(sz) {
				*moofBuf = make([]byte, sz)
			}
			buf := (*moofBuf)[:sz]
			copy(buf[0:8], hdr[:])
			if _, err := io.ReadFull(br, buf[8:]); err != nil {
				return fileMaxEnd, nil
			}
			filePos += payloadLen
			endDT := patchMoof(buf, *seqNum, offsetMS)
			*seqNum++
			if endDT > fileMaxEnd {
				fileMaxEnd = endDT
			}
			if _, err := wt.Write(buf); err != nil {
				return fileMaxEnd, err
			}
		case "mdat":
			if fileSize >= 0 && filePos+payloadLen > fileSize {
				rem := fileSize - filePos
				if rem < 0 {
					return fileMaxEnd, nil
				}
				payloadLen = rem
				binary.BigEndian.PutUint32(hdr[0:4], uint32(8+payloadLen))
			}
			if _, err := wt.Write(hdr[:]); err != nil {
				return fileMaxEnd, err
			}
			n, err := io.CopyBuffer(wt, io.LimitReader(br, payloadLen), copyBuf)
			filePos += n
			if wt.err != nil {
				return fileMaxEnd, wt.err
			}
			if n < payloadLen {
				if perr := padZeroes(wt, payloadLen-n, copyBuf); perr != nil {
					return fileMaxEnd, perr
				}
				return fileMaxEnd, nil
			}
			if err != nil {
				return fileMaxEnd, nil
			}
		default:
			n, err := io.CopyBuffer(io.Discard, io.LimitReader(br, payloadLen), copyBuf)
			filePos += n
			if err != nil || n < payloadLen {
				return fileMaxEnd, nil
			}
		}
	}
}

func mergeMP4Clips(w io.Writer, files []string) error {
	var seqNum uint32 = 1
	var offsetMS uint64 = 0
	copyBuf := make([]byte, 32*1024)
	var moofBuf []byte
	wroteInit := false

	for _, path := range files {
		nextOffset, err := mergeOneClip(w, path, copyBuf, &moofBuf, &wroteInit, &seqNum, offsetMS)
		if err != nil {
			return err
		}
		offsetMS = nextOffset
	}
	return nil
}

func uploadMergedBlock(cfg *Config, dayFolderID, blockName string, files []string) (string, error) {
	dropPageCache()
	defer dropPageCache()

	remoteName := blockName + ".mp4"
	uploadURL, err := initResumableUpload(cfg, dayFolderID, remoteName, -1)
	if err != nil {
		return "", err
	}

	pr, pw := io.Pipe()
	defer pr.Close()
	go func() {
		_ = pw.CloseWithError(mergeMP4Clips(pw, files))
	}()

	return uploadStreamResumable(uploadURL, pr)
}

type ClipInfo struct {
	FullPath  string
	RelKey    string
	MonthName string
	DayName   string
	Hour      int
	BlockIdx  int
	BlockName string
	BlockKey  string
	LiveName  string
}

func blockLabel(blockIdx int) string {
	start := blockIdx * 3
	end := start + 3
	return fmt.Sprintf("%02d-%02d", start, end)
}

func scanClips(mediaDir string, scanHrs int, tzOffsetMin int) ([]ClipInfo, error) {
	cutoffYM := ""
	cutoffYMD := ""
	now := cameraNow(tzOffsetMin)
	if now.Year() >= 2025 && scanHrs > 0 {
		cut := now.Add(-time.Duration(scanHrs) * time.Hour)
		cutoffYM = fmt.Sprintf("%04d%02d", cut.Year(), int(cut.Month()))
		cutoffYMD = fmt.Sprintf("%04d%02d%02d", cut.Year(), int(cut.Month()), cut.Day())
	}

	var clips []ClipInfo
	err := filepath.Walk(mediaDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			if path == mediaDir {
				return nil
			}
			rel, rerr := filepath.Rel(mediaDir, path)
			if rerr != nil {
				return nil
			}
			parts := strings.Split(filepath.ToSlash(rel), "/")
			if len(parts) == 1 {
				ym := parts[0]
				if len(ym) != 6 || (cutoffYM != "" && ym < cutoffYM) {
					return filepath.SkipDir
				}
			} else if len(parts) == 2 {
				ym := parts[0]
				dayStr := strings.TrimSuffix(parts[1], "d")
				if len(dayStr) == 1 {
					dayStr = "0" + dayStr
				}
				ymd := ym + dayStr
				if cutoffYMD != "" && ymd < cutoffYMD {
					return filepath.SkipDir
				}
			}
			return nil
		}
		name := info.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(strings.ToLower(name), ".mp4") {
			return nil
		}
		if info.Size() < 64*1024 {
			return nil
		}
		rel, err := filepath.Rel(mediaDir, path)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		parts := strings.Split(relSlash, "/")
		if len(parts) < 4 {
			return nil
		}
		ym := parts[0]
		monthName := ym
		if len(ym) == 6 {
			monthName = ym[:4] + "-" + ym[4:6]
		}
		dayName := strings.TrimSuffix(parts[1], "d")
		if len(dayName) == 1 {
			dayName = "0" + dayName
		}
		hrStr := strings.TrimSuffix(parts[2], "h")
		hr, err := strconv.Atoi(hrStr)
		if err != nil || hr < 0 || hr > 23 {
			return nil
		}
		bIdx := hr / 3
		bName := blockLabel(bIdx)
		clips = append(clips, ClipInfo{
			FullPath:  path,
			RelKey:    relSlash,
			MonthName: monthName,
			DayName:   dayName,
			Hour:      hr,
			BlockIdx:  bIdx,
			BlockName: bName,
			BlockKey:  fmt.Sprintf("BLOCK:%s/%s/%s", monthName, dayName, bName),
			LiveName:  fmt.Sprintf("%02dh_%s", hr, name),
		})
		return nil
	})
	sort.Slice(clips, func(i, j int) bool {
		return clips[i].RelKey < clips[j].RelKey
	})
	return clips, err
}

func currentBlockKey(clips []ClipInfo, tzOffsetMin int) string {
	now := cameraNow(tzOffsetMin)
	if now.Year() >= 2025 {
		monthName := fmt.Sprintf("%04d-%02d", now.Year(), int(now.Month()))
		dayName := fmt.Sprintf("%02d", now.Day())
		bName := blockLabel(now.Hour() / 3)
		return fmt.Sprintf("BLOCK:%s/%s/%s", monthName, dayName, bName)
	}
	if len(clips) > 0 {
		return clips[len(clips)-1].BlockKey
	}
	return ""
}

func loadUploadedSet(path string) map[string]bool {
	set := make(map[string]bool)
	b, err := os.ReadFile(path)
	if err != nil {
		return set
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			set[line] = true
		}
	}
	return set
}

func appendUploaded(path, key string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(key + "\n")
	f.Close()

	// Keep /conf/uploaded_v2.log small (< 8 KB) so the 384 KB /conf JFFS2 partition never fills up over months
	if st, err := os.Stat(path); err == nil && st.Size() > 8192 {
		if b, err := os.ReadFile(path); err == nil {
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(lines) > 120 {
				var kept []string
				for _, l := range lines[:len(lines)-120] {
					if strings.HasPrefix(l, "BLOCK:") {
						kept = append(kept, l)
					}
				}
				if len(kept) > 60 {
					kept = kept[len(kept)-60:]
				}
				kept = append(kept, lines[len(lines)-120:]...)
				tmp := path + ".tmp"
				if os.WriteFile(tmp, []byte(strings.Join(kept, "\n")+"\n"), 0644) == nil {
					_ = os.Rename(tmp, path)
				}
			}
		}
	}
}

var lastRetentionCheck time.Time

func deleteOldDriveFiles(cfg *Config, rootFolderID string, retentionHrs int, tzOffsetMin int) {
	if retentionHrs <= 0 || rootFolderID == "" {
		return
	}
	if !lastRetentionCheck.IsZero() && time.Since(lastRetentionCheck) < time.Hour {
		return
	}
	now := cameraNow(tzOffsetMin)
	if now.Year() < 2025 {
		return
	}
	cut := now.Add(-time.Duration(retentionHrs) * time.Hour)
	cutoffYMD := fmt.Sprintf("%04d-%02d-%02d", cut.Year(), int(cut.Month()), cut.Day())
	cutoffYM := fmt.Sprintf("%04d-%02d", cut.Year(), int(cut.Month()))

	// List YYYY-MM month folders strictly inside CCTV/ (rootFolderID)
	mq := fmt.Sprintf("mimeType = 'application/vnd.google-apps.folder' and '%s' in parents and trashed = false", escapeDriveQueryValue(rootFolderID))
	months, err := driveListFiles(cfg, mq, "files(id,name)")
	if err != nil {
		logf("[err] retention list months: %v", err)
		return
	}

	hadError := false
	for _, mf := range months {
		if len(mf.Name) != 7 || mf.Name[4] != '-' || mf.Name > cutoffYM {
			continue
		}
		dq := fmt.Sprintf("mimeType = 'application/vnd.google-apps.folder' and '%s' in parents and trashed = false", escapeDriveQueryValue(mf.ID))
		days, err := driveListFiles(cfg, dq, "files(id,name)")
		if err != nil {
			logf("[err] retention list days %s: %v", mf.Name, err)
			hadError = true
			continue
		}
		remaining := 0
		for _, df := range days {
			dayName := df.Name
			if len(dayName) == 1 {
				dayName = "0" + dayName
			}
			ymd := mf.Name + "-" + dayName
			if len(dayName) == 2 && ymd < cutoffYMD {
				if err := deleteDriveObject(cfg, df.ID); err != nil {
					logf("[err] retention delete day %s/%s (%s): %v", mf.Name, df.Name, df.ID, err)
					hadError = true
					remaining++
				} else {
					logf("[retention] deleted expired day folder %s/%s (%s)", mf.Name, df.Name, df.ID)
				}
			} else {
				remaining++
			}
		}
		if remaining == 0 && mf.Name < cutoffYM {
			if err := deleteDriveObject(cfg, mf.ID); err != nil {
				logf("[err] retention delete month %s (%s): %v", mf.Name, mf.ID, err)
				hadError = true
			} else {
				logf("[retention] deleted empty expired month folder %s (%s)", mf.Name, mf.ID)
			}
		}
	}
	if !hadError {
		lastRetentionCheck = time.Now()
	}
}

func cleanupLiveFolder(cfg *Config, dayID, blockName string) {
	q := fmt.Sprintf(
		"mimeType = 'application/vnd.google-apps.folder' and name = '%s' and '%s' in parents and trashed = false",
		escapeDriveQueryValue("live_"+blockName),
		escapeDriveQueryValue(dayID),
	)
	folders, err := driveListFiles(cfg, q, "files(id,name)")
	if err != nil {
		logf("[err] cleanup lookup live_%s: %v", blockName, err)
		return
	}
	for _, lf := range folders {
		if err := deleteDriveObject(cfg, lf.ID); err != nil {
			logf("[err] cleanup remove %s (%s): %v", lf.Name, lf.ID, err)
		} else {
			logf("[cleanup] removed temporary folder %s (%s)", lf.Name, lf.ID)
		}
	}
}

func main() {
	debug.SetGCPercent(20)
	debug.SetMemoryLimit(5 * 1024 * 1024)

	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--selftest" {
		caFile := ""
		if len(args) > 1 {
			caFile = args[1]
		}
		if err := initHTTP(caFile); err != nil {
			fmt.Println("initHTTP err:", err)
			os.Exit(1)
		}
		resp, err := httpClient.Get(driveAPIBase + "/discovery/v1/apis/drive/v3/rest")
		if err != nil {
			fmt.Println("HTTPS test err:", err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		fmt.Printf("SELFTEST_OK status=%d arch=%s/%s\n", resp.StatusCode, runtime.GOOS, runtime.GOARCH)
		return
	}

	if len(args) >= 2 && args[0] == "--log" {
		logMu.Lock()
		logFilePath = args[1]
		logMu.Unlock()
		args = args[2:]
	}

	cfgPath := "/conf/gdrive.json"
	if len(args) > 0 {
		cfgPath = args[0]
	}

	var lastClientID, lastClientSecret, lastRefreshToken string

	for {
		cfgBytes, err := os.ReadFile(cfgPath)
		if err != nil {
			logf("[wait] waiting for config %s: %v", cfgPath, err)
			sleepFunc(15 * time.Second)
			continue
		}
		var cfg Config
		if err := json.Unmarshal(cfgBytes, &cfg); err != nil {
			logf("[err] invalid config JSON: %v", err)
			sleepFunc(15 * time.Second)
			continue
		}
		if cfg.MediaDir == "" {
			cfg.MediaDir = "/mnt/sdcard/Media"
		}
		if cfg.StateFile == "" {
			cfg.StateFile = "/conf/uploaded_v2.log"
		}
		if cfg.CACertFile == "" {
			cfg.CACertFile = "/tmp/tools/cacert.pem"
		}
		if cfg.RetentionHrs == 0 {
			cfg.RetentionHrs = 72
		}
		if cfg.PollSeconds <= 0 {
			cfg.PollSeconds = 20
		}
		if err := validateConfig(&cfg); err != nil {
			logf("[err] invalid config: %v", err)
			sleepFunc(15 * time.Second)
			continue
		}
		if cfg.ClientID != lastClientID || cfg.ClientSecret != lastClientSecret || cfg.RefreshToken != lastRefreshToken {
			invalidateToken()
			lastClientID = cfg.ClientID
			lastClientSecret = cfg.ClientSecret
			lastRefreshToken = cfg.RefreshToken
		}
		if httpClient == nil {
			if err := initHTTP(cfg.CACertFile); err != nil {
				logf("[err] initHTTP: %v", err)
				sleepFunc(15 * time.Second)
				continue
			}
		}

		tzOff := tzOffsetMinutes(&cfg)
		scanHrs := 72
		if cfg.RetentionHrs > 0 && cfg.RetentionHrs < scanHrs {
			scanHrs = cfg.RetentionHrs
		}
		uploaded := loadUploadedSet(cfg.StateFile)
		clips, scanErr := scanClips(cfg.MediaDir, scanHrs, tzOff)
		if scanErr != nil {
			logf("[warn] scanClips %s: %v", cfg.MediaDir, scanErr)
		}
		activeBlock := currentBlockKey(clips, tzOff)

		blockMap := make(map[string][]ClipInfo)
		var blockOrder []string
		for _, c := range clips {
			if _, exists := blockMap[c.BlockKey]; !exists {
				blockOrder = append(blockOrder, c.BlockKey)
			}
			blockMap[c.BlockKey] = append(blockMap[c.BlockKey], c)
		}

		if _, err := getAccessToken(&cfg); err != nil {
			logf("[err] token: %v", err)
			sleepFunc(time.Duration(cfg.PollSeconds) * time.Second)
			continue
		}

		rootID := cfg.FolderID
		if rootID == "" {
			rootID, err = ensureSubfolder(&cfg, "", cfg.FolderName)
			if err != nil {
				logf("[err] root folder: %v", err)
				sleepFunc(time.Duration(cfg.PollSeconds) * time.Second)
				continue
			}
		}

		// 1. Current active 3-hour block -> upload each new 5-minute clip immediately to CCTV / YYYY-MM / DD / live_<block> /
		liveFailed := false
		if activeClips, ok := blockMap[activeBlock]; ok {
			for i := len(activeClips) - 1; i >= 0; i-- {
				c := activeClips[i]
				if uploaded[c.RelKey] {
					continue
				}
				monthID, err := ensureSubfolder(&cfg, rootID, c.MonthName)
				if err != nil {
					logf("[err] month folder %s: %v", c.MonthName, err)
					liveFailed = true
					break
				}
				dayID, err := ensureSubfolder(&cfg, monthID, c.DayName)
				if err != nil {
					logf("[err] day folder %s: %v", c.DayName, err)
					liveFailed = true
					break
				}
				liveFolderID, err := ensureSubfolder(&cfg, dayID, "live_"+c.BlockName)
				if err != nil {
					logf("[err] live folder %s: %v", c.BlockName, err)
					liveFailed = true
					break
				}
				logf("[live] uploading %s/%s/live_%s/%s ...", c.MonthName, c.DayName, c.BlockName, c.LiveName)
				t0 := time.Now()
				id, err := uploadSingleFile(&cfg, liveFolderID, c.FullPath, c.LiveName)
				if err != nil {
					logf("[err] live upload %s: %v", c.LiveName, err)
					liveFailed = true
					break
				}
				logf("[ok-live] uploaded %s -> id=%s in %s", c.LiveName, id, time.Since(t0).Round(time.Millisecond))
				uploaded[c.RelKey] = true
				appendUploaded(cfg.StateFile, c.RelKey)
				runtime.GC()
			}
		}

		// 2. Completed 3-hour blocks -> stream-merge into CCTV / YYYY-MM / DD / HH-HH.mp4
		if !liveFailed {
			for _, bKey := range blockOrder {
				if bKey == activeBlock || uploaded[bKey] {
					continue
				}
				bClips := blockMap[bKey]
				if len(bClips) == 0 {
					continue
				}
				c0 := bClips[0]
				monthID, err := ensureSubfolder(&cfg, rootID, c0.MonthName)
				if err != nil {
					logf("[err] month folder %s: %v", c0.MonthName, err)
					break
				}
				dayID, err := ensureSubfolder(&cfg, monthID, c0.DayName)
				if err != nil {
					logf("[err] day folder %s: %v", c0.DayName, err)
					break
				}
				remoteName := c0.BlockName + ".mp4"
				if exists, err := driveFileExists(&cfg, dayID, remoteName); err == nil && exists {
					logf("[skip-merge] %s/%s/%s already exists in Drive", c0.MonthName, c0.DayName, remoteName)
					uploaded[bKey] = true
					appendUploaded(cfg.StateFile, bKey)
					cleanupLiveFolder(&cfg, dayID, c0.BlockName)
					continue
				}
				var filePaths []string
				for _, c := range bClips {
					filePaths = append(filePaths, c.FullPath)
				}
				logf("[merge] stream-merging %d clips -> %s/%s/%s ...", len(filePaths), c0.MonthName, c0.DayName, remoteName)
				t0 := time.Now()
				id, err := uploadMergedBlock(&cfg, dayID, c0.BlockName, filePaths)
				if err != nil {
					logf("[err] merge %s: %v", bKey, err)
					break
				}
				logf("[ok-merge] uploaded %s/%s/%s -> id=%s in %s", c0.MonthName, c0.DayName, remoteName, id, time.Since(t0).Round(time.Millisecond))
				uploaded[bKey] = true
				appendUploaded(cfg.StateFile, bKey)

				// Delete temporary live_<block> folder once the merged 3-hour video is uploaded
				cleanupLiveFolder(&cfg, dayID, c0.BlockName)
				runtime.GC()
				break
			}
		}

		deleteOldDriveFiles(&cfg, rootID, cfg.RetentionHrs, tzOff)
		runtime.GC()
		sleepFunc(time.Duration(cfg.PollSeconds) * time.Second)
	}
}
