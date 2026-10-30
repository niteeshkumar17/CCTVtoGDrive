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
	"time"
)

type Config struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
	FolderID     string `json:"folder_id"`
	FolderName   string `json:"folder_name"`
	RetentionHrs int    `json:"retention_hours"`
	MediaDir     string `json:"media_dir"`
	StateFile    string `json:"state_file"`
	CACertFile   string `json:"ca_cert_file"`
	PollSeconds  int    `json:"poll_seconds"`
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
	Files []DriveFile `json:"files"`
}

const scottyChunkSize = 512 * 1024 // 524,288 bytes (2 * 256 KiB, required by Google Scotty)

var (
	httpClient  *http.Client
	cachedToken string
	tokenExpiry time.Time
	folderCache = make(map[string]string)
	scottyBuf   = make([]byte, scottyChunkSize)
)

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
	} else {
		tlsCfg.InsecureSkipVerify = true
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
	resp, err := httpClient.PostForm("https://oauth2.googleapis.com/token", vals)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("token HTTP %d", resp.StatusCode)
	}
	var tr TokenResp
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", err
	}
	cachedToken = tr.AccessToken
	exp := tr.ExpiresIn
	if exp <= 120 {
		exp = 3000
	}
	tokenExpiry = time.Now().Add(time.Duration(exp-120) * time.Second)
	return cachedToken, nil
}

func ensureSubfolder(token, parentID, name string) (string, error) {
	cacheKey := parentID + "/" + name
	if id, ok := folderCache[cacheKey]; ok && id != "" {
		return id, nil
	}
	q := fmt.Sprintf("mimeType = 'application/vnd.google-apps.folder' and name = '%s' and trashed = false", name)
	if parentID != "" {
		q += fmt.Sprintf(" and '%s' in parents", parentID)
	}
	u := "https://www.googleapis.com/drive/v3/files?pageSize=1&fields=files(id,name)&q=" + url.QueryEscape(q)
	gotEmptyList := false
	for try := 0; try < 100; try++ {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := httpClient.Do(req)
		if err != nil {
			return "", err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 {
			var list DriveListResp
			if json.Unmarshal(body, &list) == nil && len(list.Files) > 0 {
				folderCache[cacheKey] = list.Files[0].ID
				return list.Files[0].ID, nil
			}
			gotEmptyList = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !gotEmptyList {
		return "", fmt.Errorf("ensureSubfolder(%s) GET rate-limited", name)
	}
	meta := map[string]interface{}{
		"name":     name,
		"mimeType": "application/vnd.google-apps.folder",
	}
	if parentID != "" {
		meta["parents"] = []string{parentID}
	}
	mb, _ := json.Marshal(meta)
	for try := 0; try < 50; try++ {
		creq, _ := http.NewRequest("POST", "https://www.googleapis.com/drive/v3/files", bytes.NewReader(mb))
		creq.Header.Set("Authorization", "Bearer "+token)
		creq.Header.Set("Content-Type", "application/json")
		cresp, err := httpClient.Do(creq)
		if err != nil {
			return "", err
		}
		cbody, _ := io.ReadAll(cresp.Body)
		cresp.Body.Close()
		if cresp.StatusCode == 200 || cresp.StatusCode == 201 {
			var df DriveFile
			if err := json.Unmarshal(cbody, &df); err != nil {
				return "", err
			}
			folderCache[cacheKey] = df.ID
			return df.ID, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "", fmt.Errorf("ensureSubfolder(%s) rate-limited", name)
}

func deleteDriveObject(token, fileID string) {
	if fileID == "" {
		return
	}
	for try := 0; try < 30; try++ {
		req, _ := http.NewRequest("DELETE", "https://www.googleapis.com/drive/v3/files/"+fileID, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := httpClient.Do(req)
		if err != nil {
			return
		}
		resp.Body.Close()
		if resp.StatusCode == 204 || resp.StatusCode == 200 || resp.StatusCode == 404 {
			for k, v := range folderCache {
				if v == fileID {
					delete(folderCache, k)
				}
			}
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func initResumableUpload(token, folderID, remoteName string, totalSize int64) (string, error) {
	meta := map[string]interface{}{
		"name": remoteName,
	}
	if folderID != "" {
		meta["parents"] = []string{folderID}
	}
	metaBytes, _ := json.Marshal(meta)

	for try := 0; try < 60; try++ {
		initReq, err := http.NewRequest("POST", "https://www.googleapis.com/upload/drive/v3/files?uploadType=resumable", bytes.NewReader(metaBytes))
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
			return "", fmt.Errorf("init resumable: %w", err)
		}
		_, _ = io.Copy(io.Discard, initResp.Body)
		initResp.Body.Close()
		if initResp.StatusCode == 200 {
			loc := initResp.Header.Get("Location")
			if loc != "" {
				return loc, nil
			}
		}
		if initResp.StatusCode == 401 {
			cachedToken = ""
			return "", fmt.Errorf("token expired (401)")
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "", fmt.Errorf("failed to get resumable upload URL after retries")
}

// uploadStreamResumable streams any io.Reader to uploadURL in 512 KB Scotty chunks (308 Resume Incomplete)
// using a 1-byte lookahead, and retries the final chunk from RAM up to 60 times if Drive returns 403.
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
					time.Sleep(time.Second)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode == 308 || resp.StatusCode == 200 || resp.StatusCode == 201 {
					ok = true
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			if !ok {
				return "", fmt.Errorf("intermediate chunk failed at offset %d", offset)
			}
			offset += int64(filled)
			dropPageCache()
		} else {
			totalSize := offset + int64(filled)
			contentRange := fmt.Sprintf("bytes %d-%d/%d", offset, totalSize-1, totalSize)
			queryRange := fmt.Sprintf("bytes */%d", totalSize)
			sendZeroByte := false
			for try := 0; try < 120; try++ {
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
					time.Sleep(time.Second)
					continue
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode == 200 || resp.StatusCode == 201 {
					var df DriveFile
					_ = json.Unmarshal(body, &df)
					return df.ID, nil
				}
				if resp.StatusCode == 308 {
					sendZeroByte = false
				} else {
					// Toggle between re-sending final chunk and 0-byte commit query
					sendZeroByte = !sendZeroByte
				}
				time.Sleep(500 * time.Millisecond)
			}
			return "", fmt.Errorf("final commit chunk rate-limited after 120 retries")
		}
	}
}

func uploadSingleFile(token, folderID, localPath, remoteName string) (string, error) {
	dropPageCache()
	defer dropPageCache()

	if _, err := os.Stat(localPath); err != nil {
		return "", err
	}
	uploadURL, err := initResumableUpload(token, folderID, remoteName, -1)
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

func uploadMergedBlock(token, dayFolderID, blockName string, files []string) (string, error) {
	dropPageCache()
	defer dropPageCache()

	remoteName := blockName + ".mp4"
	uploadURL, err := initResumableUpload(token, dayFolderID, remoteName, -1)
	if err != nil {
		return "", err
	}

	pr, pw := io.Pipe()
	go func() {
		var seqNum uint32 = 1
		var offsetMS uint64 = 0
		copyBuf := make([]byte, 32*1024)
		var hdr [8]byte
		wroteInit := false

		for _, path := range files {
			dropPageCache()
			f, err := os.Open(path)
			if err != nil {
				continue
			}
			br := bufio.NewReaderSize(&cacheDroppingReader{r: f}, 32*1024)
			fileMaxEnd := offsetMS

			for {
				if _, err := io.ReadFull(br, hdr[:]); err != nil {
					break
				}
				sz := int64(binary.BigEndian.Uint32(hdr[0:4]))
				typ := string(hdr[4:8])
				if sz < 8 || sz > 32*1024*1024 {
					break
				}
				payloadLen := sz - 8

				switch typ {
				case "ftyp", "free", "moov":
					if !wroteInit {
						if _, err := pw.Write(hdr[:]); err != nil {
							f.Close()
							_ = pw.CloseWithError(err)
							return
						}
						if _, err := io.CopyBuffer(pw, io.LimitReader(br, payloadLen), copyBuf); err != nil {
							f.Close()
							_ = pw.CloseWithError(err)
							return
						}
						if typ == "moov" {
							wroteInit = true
						}
					} else {
						if _, err := io.CopyBuffer(io.Discard, io.LimitReader(br, payloadLen), copyBuf); err != nil {
							break
						}
					}
				case "moof":
					moofBuf := make([]byte, sz)
					copy(moofBuf[0:8], hdr[:])
					if _, err := io.ReadFull(br, moofBuf[8:]); err != nil {
						break
					}
					endDT := patchMoof(moofBuf, seqNum, offsetMS)
					seqNum++
					if endDT > fileMaxEnd {
						fileMaxEnd = endDT
					}
					if _, err := pw.Write(moofBuf); err != nil {
						f.Close()
						_ = pw.CloseWithError(err)
						return
					}
				case "mdat":
					if _, err := pw.Write(hdr[:]); err != nil {
						f.Close()
						_ = pw.CloseWithError(err)
						return
					}
					if _, err := io.CopyBuffer(pw, io.LimitReader(br, payloadLen), copyBuf); err != nil {
						f.Close()
						_ = pw.CloseWithError(err)
						return
					}
				default:
					if _, err := io.CopyBuffer(io.Discard, io.LimitReader(br, payloadLen), copyBuf); err != nil {
						break
					}
				}
			}
			f.Close()
			dropPageCache()
			offsetMS = fileMaxEnd
		}
		_ = pw.Close()
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

func scanClips(mediaDir string, retentionHrs int) ([]ClipInfo, error) {
	cutoffYM := ""
	cutoffYMD := ""
	ist := time.Now().UTC().Add(5*time.Hour + 30*time.Minute)
	if ist.Year() >= 2025 && retentionHrs > 0 {
		cut := ist.Add(-time.Duration(retentionHrs) * time.Hour)
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

func currentISTBlockKey(clips []ClipInfo) string {
	ist := time.Now().UTC().Add(5*time.Hour + 30*time.Minute)
	if ist.Year() >= 2025 {
		monthName := fmt.Sprintf("%04d-%02d", ist.Year(), int(ist.Month()))
		dayName := fmt.Sprintf("%02d", ist.Day())
		bName := blockLabel(ist.Hour() / 3)
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

func deleteOldDriveFiles(token, rootFolderID string, retentionHrs int) {
	if retentionHrs <= 0 || rootFolderID == "" {
		return
	}
	if !lastRetentionCheck.IsZero() && time.Since(lastRetentionCheck) < time.Hour {
		return
	}
	ist := time.Now().UTC().Add(5*time.Hour + 30*time.Minute)
	if ist.Year() < 2025 {
		return
	}
	lastRetentionCheck = time.Now()
	cut := ist.Add(-time.Duration(retentionHrs) * time.Hour)
	cutoffYMD := fmt.Sprintf("%04d-%02d-%02d", cut.Year(), int(cut.Month()), cut.Day())
	cutoffYM := fmt.Sprintf("%04d-%02d", cut.Year(), int(cut.Month()))

	// List YYYY-MM month folders strictly inside CCTV/ (rootFolderID)
	mq := fmt.Sprintf("mimeType = 'application/vnd.google-apps.folder' and '%s' in parents and trashed = false", rootFolderID)
	mu := "https://www.googleapis.com/drive/v3/files?pageSize=50&fields=files(id,name)&q=" + url.QueryEscape(mq)
	mreq, _ := http.NewRequest("GET", mu, nil)
	mreq.Header.Set("Authorization", "Bearer "+token)
	mresp, err := httpClient.Do(mreq)
	if err != nil {
		return
	}
	mbody, _ := io.ReadAll(mresp.Body)
	mresp.Body.Close()
	var mlist DriveListResp
	if json.Unmarshal(mbody, &mlist) != nil {
		return
	}
	for _, mf := range mlist.Files {
		if len(mf.Name) != 7 || mf.Name[4] != '-' || mf.Name > cutoffYM {
			continue
		}
		dq := fmt.Sprintf("mimeType = 'application/vnd.google-apps.folder' and '%s' in parents and trashed = false", mf.ID)
		du := "https://www.googleapis.com/drive/v3/files?pageSize=50&fields=files(id,name)&q=" + url.QueryEscape(dq)
		dreq, _ := http.NewRequest("GET", du, nil)
		dreq.Header.Set("Authorization", "Bearer "+token)
		dresp, err := httpClient.Do(dreq)
		if err != nil {
			continue
		}
		dbody, _ := io.ReadAll(dresp.Body)
		dresp.Body.Close()
		var dlist DriveListResp
		if json.Unmarshal(dbody, &dlist) != nil {
			continue
		}
		remaining := 0
		for _, df := range dlist.Files {
			dayName := df.Name
			if len(dayName) == 1 {
				dayName = "0" + dayName
			}
			ymd := mf.Name + "-" + dayName
			if len(dayName) == 2 && ymd < cutoffYMD {
				deleteDriveObject(token, df.ID)
				fmt.Printf("[retention] deleted expired day folder %s/%s (%s)\n", mf.Name, df.Name, df.ID)
			} else {
				remaining++
			}
		}
		if remaining == 0 && mf.Name < cutoffYM {
			deleteDriveObject(token, mf.ID)
			fmt.Printf("[retention] deleted empty expired month folder %s (%s)\n", mf.Name, mf.ID)
		}
	}
}

func main() {
	debug.SetGCPercent(20)
	debug.SetMemoryLimit(5 * 1024 * 1024)

	if len(os.Args) > 1 && os.Args[1] == "--selftest" {
		caFile := ""
		if len(os.Args) > 2 {
			caFile = os.Args[2]
		}
		if err := initHTTP(caFile); err != nil {
			fmt.Println("initHTTP err:", err)
			os.Exit(1)
		}
		resp, err := httpClient.Get("https://www.googleapis.com/discovery/v1/apis/drive/v3/rest")
		if err != nil {
			fmt.Println("HTTPS test err:", err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		fmt.Printf("SELFTEST_OK status=%d arch=%s/%s\n", resp.StatusCode, runtime.GOOS, runtime.GOARCH)
		return
	}

	cfgPath := "/conf/gdrive.json"
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}

	for {
		dropPageCache()
		cfgBytes, err := os.ReadFile(cfgPath)
		if err != nil {
			fmt.Printf("[wait] waiting for config %s: %v\n", cfgPath, err)
			time.Sleep(15 * time.Second)
			continue
		}
		var cfg Config
		if err := json.Unmarshal(cfgBytes, &cfg); err != nil {
			fmt.Printf("[err] invalid config JSON: %v\n", err)
			time.Sleep(15 * time.Second)
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
			cfg.RetentionHrs = 2160
		}
		if cfg.PollSeconds <= 0 {
			cfg.PollSeconds = 20
		}
		if httpClient == nil {
			if err := initHTTP(cfg.CACertFile); err != nil {
				fmt.Printf("[err] initHTTP: %v\n", err)
				time.Sleep(15 * time.Second)
				continue
			}
		}

		uploaded := loadUploadedSet(cfg.StateFile)
		clips, _ := scanClips(cfg.MediaDir, 72)
		activeBlock := currentISTBlockKey(clips)

		blockMap := make(map[string][]ClipInfo)
		var blockOrder []string
		for _, c := range clips {
			if _, exists := blockMap[c.BlockKey]; !exists {
				blockOrder = append(blockOrder, c.BlockKey)
			}
			blockMap[c.BlockKey] = append(blockMap[c.BlockKey], c)
		}

		token, err := getAccessToken(&cfg)
		if err != nil {
			fmt.Printf("[err] token: %v\n", err)
			time.Sleep(time.Duration(cfg.PollSeconds) * time.Second)
			continue
		}

		rootID := cfg.FolderID
		if rootID == "" {
			rootID, err = ensureSubfolder(token, "", cfg.FolderName)
			if err != nil {
				fmt.Printf("[err] root folder: %v\n", err)
				time.Sleep(time.Duration(cfg.PollSeconds) * time.Second)
				continue
			}
		}

		// 1. Current active 3-hour block -> upload each new 5-minute clip immediately to CCTV / YYYY-MM / DD / live_<block> /
		if activeClips, ok := blockMap[activeBlock]; ok {
			for i := len(activeClips) - 1; i >= 0; i-- {
				c := activeClips[i]
				if uploaded[c.RelKey] {
					continue
				}
				monthID, err := ensureSubfolder(token, rootID, c.MonthName)
				if err != nil {
					break
				}
				dayID, err := ensureSubfolder(token, monthID, c.DayName)
				if err != nil {
					break
				}
				liveFolderID, err := ensureSubfolder(token, dayID, "live_"+c.BlockName)
				if err != nil {
					break
				}
				fmt.Printf("[live] uploading %s/%s/live_%s/%s ...\n", c.MonthName, c.DayName, c.BlockName, c.LiveName)
				t0 := time.Now()
				id, err := uploadSingleFile(token, liveFolderID, c.FullPath, c.LiveName)
				if err != nil {
					fmt.Printf("[err] live upload %s: %v\n", c.LiveName, err)
					break
				}
				fmt.Printf("[ok-live] uploaded %s -> id=%s in %s\n", c.LiveName, id, time.Since(t0).Round(time.Millisecond))
				uploaded[c.RelKey] = true
				appendUploaded(cfg.StateFile, c.RelKey)
				runtime.GC()
			}
		}

		// 2. Completed 3-hour blocks -> stream-merge into CCTV / YYYY-MM / DD / HH-HH.mp4
		for _, bKey := range blockOrder {
			if bKey == activeBlock || uploaded[bKey] {
				continue
			}
			bClips := blockMap[bKey]
			if len(bClips) == 0 {
				continue
			}
			c0 := bClips[0]
			monthID, err := ensureSubfolder(token, rootID, c0.MonthName)
			if err != nil {
				fmt.Printf("[err] month folder %s: %v\n", c0.MonthName, err)
				break
			}
			dayID, err := ensureSubfolder(token, monthID, c0.DayName)
			if err != nil {
				fmt.Printf("[err] day folder %s: %v\n", c0.DayName, err)
				break
			}
			var filePaths []string
			for _, c := range bClips {
				filePaths = append(filePaths, c.FullPath)
			}
			fmt.Printf("[merge] stream-merging %d clips -> %s/%s/%s.mp4 ...\n", len(filePaths), c0.MonthName, c0.DayName, c0.BlockName)
			t0 := time.Now()
			id, err := uploadMergedBlock(token, dayID, c0.BlockName, filePaths)
			if err != nil {
				fmt.Printf("[err] merge %s: %v\n", bKey, err)
				break
			}
			fmt.Printf("[ok-merge] uploaded %s/%s/%s.mp4 -> id=%s in %s\n", c0.MonthName, c0.DayName, c0.BlockName, id, time.Since(t0).Round(time.Millisecond))
			uploaded[bKey] = true
			appendUploaded(cfg.StateFile, bKey)

			// Delete temporary live_<block> folder once the merged 3-hour video is uploaded
			q := fmt.Sprintf("mimeType = 'application/vnd.google-apps.folder' and name = 'live_%s' and '%s' in parents and trashed = false", c0.BlockName, dayID)
			u := "https://www.googleapis.com/drive/v3/files?pageSize=5&fields=files(id,name)&q=" + url.QueryEscape(q)
			req, _ := http.NewRequest("GET", u, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			if resp, err := httpClient.Do(req); err == nil {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				var list DriveListResp
				if json.Unmarshal(body, &list) == nil {
					for _, lf := range list.Files {
						deleteDriveObject(token, lf.ID)
						fmt.Printf("[cleanup] removed temporary folder %s (%s)\n", lf.Name, lf.ID)
					}
				}
			}
			runtime.GC()
			break
		}

		deleteOldDriveFiles(token, rootID, cfg.RetentionHrs)
		runtime.GC()
		time.Sleep(time.Duration(cfg.PollSeconds) * time.Second)
	}
}
