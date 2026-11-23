package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func makeBox(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(out)))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

func makeFullBox(typ string, version byte, flags uint32, payload []byte) []byte {
	buf := make([]byte, 4+len(payload))
	buf[0] = version
	buf[1] = byte(flags >> 16)
	buf[2] = byte(flags >> 8)
	buf[3] = byte(flags)
	copy(buf[4:], payload)
	return makeBox(typ, buf)
}

func makeSyntheticClip(t *testing.T, dir, name string, seq uint32, tfdt uint64, sampleDur uint32, mdatData []byte) string {
	t.Helper()
	ftyp := makeBox("ftyp", []byte("isom\x00\x00\x02\x00isom"))
	moov := makeBox("moov", makeBox("mvhd", make([]byte, 16)))

	mfhdPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(mfhdPayload, seq)
	mfhd := makeFullBox("mfhd", 0, 0, mfhdPayload)

	tfhd := makeFullBox("tfhd", 0, 0, make([]byte, 4))

	tfdtPayload := make([]byte, 8)
	binary.BigEndian.PutUint64(tfdtPayload, tfdt)
	tfdtBox := makeFullBox("tfdt", 1, 0, tfdtPayload)

	// trun with sample-duration-present (0x100), 1 sample
	trunPayload := make([]byte, 8)
	binary.BigEndian.PutUint32(trunPayload[0:4], 1)
	binary.BigEndian.PutUint32(trunPayload[4:8], sampleDur)
	trun := makeFullBox("trun", 0, 0x100, trunPayload)

	traf := makeBox("traf", append(append(tfhd, tfdtBox...), trun...))
	moof := makeBox("moof", append(mfhd, traf...))
	mdat := makeBox("mdat", mdatData)

	var all []byte
	all = append(all, ftyp...)
	all = append(all, moov...)
	all = append(all, moof...)
	all = append(all, mdat...)

	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, all, 0644); err != nil {
		t.Fatalf("write clip: %v", err)
	}
	return p
}

func TestPatchMoofAndMergeMP4Clips(t *testing.T) {
	dir := t.TempDir()
	c1 := makeSyntheticClip(t, dir, "c1.mp4", 7, 100, 3000, []byte("FRAME_ONE"))
	c2 := makeSyntheticClip(t, dir, "c2.mp4", 9, 50, 2000, []byte("FRAME_TWO"))

	var out bytes.Buffer
	if err := mergeMP4Clips(&out, []string{c1, c2}); err != nil {
		t.Fatalf("mergeMP4Clips err: %v", err)
	}

	raw := out.Bytes()
	var boxes []string
	var moofs [][]byte
	var mdats [][]byte
	for pos := 0; pos+8 <= len(raw); {
		sz := int(binary.BigEndian.Uint32(raw[pos : pos+4]))
		typ := string(raw[pos+4 : pos+8])
		if sz < 8 || pos+sz > len(raw) {
			t.Fatalf("invalid box at %d: sz=%d", pos, sz)
		}
		boxes = append(boxes, typ)
		if typ == "moof" {
			moofs = append(moofs, raw[pos:pos+sz])
		} else if typ == "mdat" {
			mdats = append(mdats, raw[pos+8:pos+sz])
		}
		pos += sz
	}

	wantBoxes := []string{"ftyp", "moov", "moof", "mdat", "moof", "mdat"}
	if strings.Join(boxes, ",") != strings.Join(wantBoxes, ",") {
		t.Fatalf("boxes = %v, want %v", boxes, wantBoxes)
	}
	if len(mdats) != 2 || string(mdats[0]) != "FRAME_ONE" || string(mdats[1]) != "FRAME_TWO" {
		t.Fatalf("unexpected mdats: %q", mdats)
	}

	// Verify mfhd sequence numbers are 1 and 2, and clip 2 tfdt was shifted by clip 1 max end (100+3000=3100) -> 3150.
	readMoofSeqAndTFDT := func(b []byte) (uint32, uint64) {
		var seq uint32
		var dt uint64
		for p := 8; p+8 <= len(b); {
			csz := int(binary.BigEndian.Uint32(b[p : p+4]))
			ctyp := string(b[p+4 : p+8])
			if ctyp == "mfhd" {
				seq = binary.BigEndian.Uint32(b[p+12 : p+16])
			} else if ctyp == "traf" {
				for tp := p + 8; tp+8 <= p+csz; {
					tsz := int(binary.BigEndian.Uint32(b[tp : tp+4]))
					ttyp := string(b[tp+4 : tp+8])
					if ttyp == "tfdt" {
						dt = binary.BigEndian.Uint64(b[tp+12 : tp+20])
					}
					tp += tsz
				}
			}
			p += csz
		}
		return seq, dt
	}

	seq1, dt1 := readMoofSeqAndTFDT(moofs[0])
	seq2, dt2 := readMoofSeqAndTFDT(moofs[1])
	if seq1 != 1 || dt1 != 100 {
		t.Fatalf("moof1 seq=%d dt=%d, want 1/100", seq1, dt1)
	}
	if seq2 != 2 || dt2 != 3150 {
		t.Fatalf("moof2 seq=%d dt=%d, want 2/3150", seq2, dt2)
	}
}

func TestMergeMP4ClipsCorruptAndOversizedMoof(t *testing.T) {
	dir := t.TempDir()
	c1 := makeSyntheticClip(t, dir, "01_good.mp4", 1, 0, 1000, []byte("AAA"))

	// Corrupt clip with a 20 MB moof header followed by junk (must skip cleanly without allocating 20 MB).
	badPath := filepath.Join(dir, "02_bad.mp4")
	var badHdr [8]byte
	binary.BigEndian.PutUint32(badHdr[0:4], 20*1024*1024)
	copy(badHdr[4:8], "moof")
	if err := os.WriteFile(badPath, append(badHdr[:], []byte("short")...), 0644); err != nil {
		t.Fatal(err)
	}

	// Truncated moof clip
	truncPath := filepath.Join(dir, "03_trunc.mp4")
	var truncHdr [8]byte
	binary.BigEndian.PutUint32(truncHdr[0:4], 256)
	copy(truncHdr[4:8], "moof")
	if err := os.WriteFile(truncPath, append(truncHdr[:], []byte("tiny")...), 0644); err != nil {
		t.Fatal(err)
	}

	c4 := makeSyntheticClip(t, dir, "04_good.mp4", 1, 0, 1000, []byte("BBB"))

	var out bytes.Buffer
	if err := mergeMP4Clips(&out, []string{c1, badPath, truncPath, c4}); err != nil {
		t.Fatalf("mergeMP4Clips unexpected error: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("AAA")) || !bytes.Contains(out.Bytes(), []byte("BBB")) {
		t.Fatalf("expected valid clips before/after corrupt clips to merge")
	}
}

func TestUploadMergedBlockEarlyCancelNoLeak(t *testing.T) {
	dir := t.TempDir()
	// Create a clip > 512 KB so uploadStreamResumable sends an intermediate chunk first.
	bigPayload := bytes.Repeat([]byte("V"), 600*1024)
	c1 := makeSyntheticClip(t, dir, "big.mp4", 1, 0, 5000, bigPayload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"tok1","expires_in":3600}`))
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/upload/"):
			w.Header().Set("Location", "http://"+r.Host+"/put-session")
			w.WriteHeader(http.StatusOK)
		case r.Method == "PUT" && r.URL.Path == "/put-session":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad request"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	oldAPI, oldTok, oldSleep := driveAPIBase, oauthTokenURL, sleepFunc
	driveAPIBase = srv.URL
	oauthTokenURL = srv.URL + "/token"
	sleepFunc = func(time.Duration) {}
	defer func() {
		driveAPIBase, oauthTokenURL, sleepFunc = oldAPI, oldTok, oldSleep
		invalidateToken()
	}()

	if err := initHTTP(""); err != nil {
		t.Fatal(err)
	}
	invalidateToken()

	cfg := &Config{ClientID: "id", ClientSecret: "sec", RefreshToken: "ref", FolderID: "root"}
	done := make(chan error, 1)
	go func() {
		_, err := uploadMergedBlock(cfg, "day1", "00-03", []string{c1})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("expected error on HTTP 400 PUT, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("uploadMergedBlock hung on early error")
	}
}

func TestScanClipsDayPaddingAndCutoff(t *testing.T) {
	media := t.TempDir()
	now := cameraNow(330)
	ym := fmt.Sprintf("%04d%02d", now.Year(), int(now.Month()))
	day := fmt.Sprintf("%dd", now.Day()) // unpadded on purpose (if day < 10, tests padding)

	p1 := filepath.Join(media, ym, day, "09h", "clip1.mp4")
	if err := os.MkdirAll(filepath.Dir(p1), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p1, make([]byte, 70*1024), 0644); err != nil {
		t.Fatal(err)
	}

	// Single-digit day folder from earlier in month or fixed 5d if within cutoff, plus definitely expired folder
	oldDate := now.Add(-120 * time.Hour)
	oldYM := fmt.Sprintf("%04d%02d", oldDate.Year(), int(oldDate.Month()))
	oldDay := fmt.Sprintf("%02dd", oldDate.Day())
	pOld := filepath.Join(media, oldYM, oldDay, "01h", "old.mp4")
	if err := os.MkdirAll(filepath.Dir(pOld), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pOld, make([]byte, 70*1024), 0644); err != nil {
		t.Fatal(err)
	}

	// Also verify explicit single-digit day padding via 5d folder when scanHrs == 0 (no cutoff)
	pFive := filepath.Join(media, "202611", "5d", "09h", "clip5.mp4")
	if err := os.MkdirAll(filepath.Dir(pFive), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pFive, make([]byte, 70*1024), 0644); err != nil {
		t.Fatal(err)
	}

	clipsCutoff, err := scanClips(media, 72, 330)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range clipsCutoff {
		if strings.Contains(c.RelKey, "old.mp4") {
			t.Fatalf("expired clip %s was not skipped", c.RelKey)
		}
	}
	if len(clipsCutoff) == 0 {
		t.Fatalf("expected current clip to be scanned")
	}
	wantDay := fmt.Sprintf("%02d", now.Day())
	if clipsCutoff[0].DayName != wantDay {
		t.Fatalf("DayName = %q, want %q", clipsCutoff[0].DayName, wantDay)
	}

	clipsAll, err := scanClips(media, 0, 330)
	if err != nil {
		t.Fatal(err)
	}
	foundFive := false
	for _, c := range clipsAll {
		if strings.HasSuffix(c.RelKey, "clip5.mp4") {
			foundFive = true
			if c.DayName != "05" || c.BlockKey != "BLOCK:2026-11/05/09-12" {
				t.Fatalf("single-digit day padding failed: %+v", c)
			}
		}
	}
	if !foundFive {
		t.Fatalf("did not find 5d clip")
	}
}

func TestAppendUploadedTrimming(t *testing.T) {
	p := filepath.Join(t.TempDir(), "uploaded_v2.log")
	for i := 0; i < 200; i++ {
		if i%20 == 0 {
			appendUploaded(p, fmt.Sprintf("BLOCK:2026-11/%02d/00-03", i/20+1))
		} else {
			appendUploaded(p, fmt.Sprintf("202611/10d/00h/clip_%03d_very_long_name_to_exceed_8kb.mp4", i))
		}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 8192 {
		t.Fatalf("ledger size %d exceeded 8192 bytes", len(b))
	}
	s := string(b)
	if !strings.Contains(s, "BLOCK:2026-11/10/00-03") || !strings.Contains(s, "clip_199_") {
		t.Fatalf("expected recent BLOCK and clip keys to be retained")
	}
}

func TestCappedLogger(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gdrive.log")
	logMu.Lock()
	oldPath := logFilePath
	logFilePath = p
	logMu.Unlock()
	defer func() {
		logMu.Lock()
		logFilePath = oldPath
		logMu.Unlock()
	}()

	for i := 0; i < 1200; i++ {
		logf("entry-%04d %s", i, strings.Repeat("x", 90))
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > maxLogBytes+256 {
		t.Fatalf("log size %d exceeded cap %d", st.Size(), maxLogBytes)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "entry-1199") {
		t.Fatalf("latest log line missing after rotation")
	}
}

func TestDriveAPIHelpers(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	deletedIDs := make(map[string]bool)
	tokenCount := 0
	putCalls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/token":
			tokenCount++
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"access_token":"tok%d","expires_in":3600}`, tokenCount)
		case r.Method == "GET" && r.URL.Path == "/drive/v3/files":
			auth := r.Header.Get("Authorization")
			if auth == "Bearer tok1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			q := r.URL.Query().Get("q")
			queries = append(queries, q)
			if strings.Contains(q, "'root_fail' in parents") {
				_, _ = w.Write([]byte(`{"files":[{"id":"m_old","name":"2025-01"}]}`))
				return
			}
			if strings.Contains(q, "'m_old' in parents") {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"boom"}`))
				return
			}
			_, _ = w.Write([]byte(`{"files":[{"id":"sub123","name":"bob's"}]}`))
		case r.Method == "DELETE":
			id := strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")
			deletedIDs[id] = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "PUT" && r.URL.Path == "/quota-err":
			putCalls++
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"errors":[{"reason":"storageQuotaExceeded"}]}}`))
		case r.Method == "PUT" && r.URL.Path == "/not-found":
			putCalls++
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"notFound"}`))
		case r.Method == "PUT" && r.URL.Path == "/retry-ok":
			putCalls++
			if putCalls == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"up_ok"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	oldAPI, oldTok, oldSleep := driveAPIBase, oauthTokenURL, sleepFunc
	driveAPIBase = srv.URL
	oauthTokenURL = srv.URL + "/token"
	sleepFunc = func(time.Duration) {}
	defer func() {
		driveAPIBase, oauthTokenURL, sleepFunc = oldAPI, oldTok, oldSleep
		invalidateToken()
		folderCache = make(map[string]string)
		lastRetentionCheck = time.Time{}
	}()

	if err := initHTTP(""); err != nil {
		t.Fatal(err)
	}
	invalidateToken()
	folderCache = make(map[string]string)
	cfg := &Config{ClientID: "id", ClientSecret: "sec", RefreshToken: "ref", FolderID: "root_fail"}

	// 1. ensureSubfolder: initial tok1 gets 401 -> refreshes to tok2 -> escapes quote in name
	id, err := ensureSubfolder(cfg, "p'1", "bob's")
	if err != nil || id != "sub123" {
		t.Fatalf("ensureSubfolder = (%q, %v), want (sub123, nil)", id, err)
	}
	if len(queries) == 0 || !strings.Contains(queries[0], `bob\'s`) || !strings.Contains(queries[0], `p\'1`) {
		t.Fatalf("expected escaped query, got %v", queries)
	}

	// 2. deleteOldDriveFiles: day listing 500 must NOT delete m_old and must NOT advance lastRetentionCheck
	lastRetentionCheck = time.Time{}
	deleteOldDriveFiles(cfg, "root_fail", 24, 330)
	if deletedIDs["m_old"] {
		t.Fatalf("month folder m_old was deleted despite day-listing 500")
	}
	if !lastRetentionCheck.IsZero() {
		t.Fatalf("lastRetentionCheck advanced despite day-listing error")
	}

	// 3. uploadStreamResumable fast-fail on storageQuotaExceeded and 404, and retry-ok on 503->200
	putCalls = 0
	if _, err := uploadStreamResumable(srv.URL+"/quota-err", bytes.NewReader([]byte("hello"))); err == nil || putCalls != 1 {
		t.Fatalf("storageQuotaExceeded should fail fast in 1 call: calls=%d err=%v", putCalls, err)
	}
	putCalls = 0
	if _, err := uploadStreamResumable(srv.URL+"/not-found", bytes.NewReader([]byte("hello"))); err == nil || putCalls != 1 {
		t.Fatalf("404 should fail fast in 1 call: calls=%d err=%v", putCalls, err)
	}
	putCalls = 0
	upID, err := uploadStreamResumable(srv.URL+"/retry-ok", bytes.NewReader([]byte("hello")))
	if err != nil || upID != "up_ok" || putCalls != 2 {
		t.Fatalf("retry-ok = (%q, %v, calls=%d), want (up_ok, nil, 2)", upID, err, putCalls)
	}

	// 4. Json unmarshal of tz_offset_minutes preserves explicit 0 vs omitted (330)
	var cDefault, cUTC Config
	if err := json.Unmarshal([]byte(`{}`), &cDefault); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"tz_offset_minutes":0}`), &cUTC); err != nil {
		t.Fatal(err)
	}
	if tzOffsetMinutes(&cDefault) != 330 || tzOffsetMinutes(&cUTC) != 0 {
		t.Fatalf("tz defaults=%d utc=%d", tzOffsetMinutes(&cDefault), tzOffsetMinutes(&cUTC))
	}
}

func TestMergeMP4ClipsTruncatedMdatClampsHeader(t *testing.T) {
	dir := t.TempDir()

	// Clip 1 has valid ftyp/moov/moof, then an mdat header claiming 4096 bytes with only 12 bytes present.
	ftyp := makeBox("ftyp", []byte("isom\x00\x00\x02\x00isom"))
	moov := makeBox("moov", makeBox("mvhd", make([]byte, 16)))
	mfhd := makeFullBox("mfhd", 0, 0, []byte{0, 0, 0, 1})
	tfhd := makeFullBox("tfhd", 0, 0, make([]byte, 4))
	tfdt := makeFullBox("tfdt", 1, 0, make([]byte, 8))
	trun := makeFullBox("trun", 0, 0x100, []byte{0, 0, 0, 1, 0, 0, 3, 232})
	moof := makeBox("moof", append(mfhd, makeBox("traf", append(append(tfhd, tfdt...), trun...))...))

	var truncMdatHdr [8]byte
	binary.BigEndian.PutUint32(truncMdatHdr[0:4], 4104)
	copy(truncMdatHdr[4:8], "mdat")
	var c1Bytes []byte
	c1Bytes = append(c1Bytes, ftyp...)
	c1Bytes = append(c1Bytes, moov...)
	c1Bytes = append(c1Bytes, moof...)
	c1Bytes = append(c1Bytes, truncMdatHdr[:]...)
	c1Bytes = append(c1Bytes, []byte("TRUNC_12BYTE")...)

	c1 := filepath.Join(dir, "01_trunc.mp4")
	if err := os.WriteFile(c1, c1Bytes, 0644); err != nil {
		t.Fatal(err)
	}
	c2 := makeSyntheticClip(t, dir, "02_ok.mp4", 1, 0, 1000, []byte("SECOND_OK"))

	var out bytes.Buffer
	if err := mergeMP4Clips(&out, []string{c1, c2}); err != nil {
		t.Fatalf("mergeMP4Clips err: %v", err)
	}

	raw := out.Bytes()
	var boxes []string
	var mdats [][]byte
	pos := 0
	for pos+8 <= len(raw) {
		sz := int(binary.BigEndian.Uint32(raw[pos : pos+4]))
		typ := string(raw[pos+4 : pos+8])
		if sz < 8 || pos+sz > len(raw) {
			t.Fatalf("broken box framing at %d: typ=%s sz=%d len=%d", pos, typ, sz, len(raw))
		}
		boxes = append(boxes, typ)
		if typ == "mdat" {
			mdats = append(mdats, raw[pos+8:pos+sz])
		}
		pos += sz
	}
	if pos != len(raw) {
		t.Fatalf("trailing unparsed bytes: pos=%d len=%d", pos, len(raw))
	}
	want := "ftyp,moov,moof,mdat,moof,mdat"
	if strings.Join(boxes, ",") != want {
		t.Fatalf("boxes = %v, want %s", boxes, want)
	}
	if len(mdats) != 2 || string(mdats[0]) != "TRUNC_12BYTE" || string(mdats[1]) != "SECOND_OK" {
		t.Fatalf("unexpected mdat payloads: %q", mdats)
	}
}

func TestFolderCacheClearOn404AndCap(t *testing.T) {
	oldCache := folderCache
	defer func() {
		folderCache = oldCache
	}()

	folderCache = make(map[string]string)
	for i := 0; i < 205; i++ {
		setFolderCache(fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i))
	}
	if len(folderCache) >= 200 {
		t.Fatalf("expected folderCache cap reset, got len=%d", len(folderCache))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"folder not found"}`))
	}))
	defer srv.Close()

	oldAPI, oldTok, oldSleep := driveAPIBase, oauthTokenURL, sleepFunc
	driveAPIBase = srv.URL
	oauthTokenURL = srv.URL + "/token"
	sleepFunc = func(time.Duration) {}
	defer func() {
		driveAPIBase, oauthTokenURL, sleepFunc = oldAPI, oldTok, oldSleep
		invalidateToken()
	}()

	if err := initHTTP(""); err != nil {
		t.Fatal(err)
	}
	invalidateToken()
	setFolderCache("root/2026-11", "stale_id")

	cfg := &Config{ClientID: "id", ClientSecret: "sec", RefreshToken: "ref", FolderID: "root"}
	if _, err := initResumableUpload(cfg, "stale_id", "00-03.mp4", 10); err == nil {
		t.Fatalf("expected 404 error")
	}
	if len(folderCache) != 0 {
		t.Fatalf("expected folderCache cleared on 404, got %v", folderCache)
	}
}

func TestDriveFileExistsAndNetworkRetry(t *testing.T) {
	var mu sync.Mutex
	getAttempts := 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
			return
		}
		if r.Method == "GET" && r.URL.Path == "/drive/v3/files" {
			mu.Lock()
			getAttempts++
			n := getAttempts
			mu.Unlock()
			if n == 1 {
				// Simulate transient network drop by hijacking and closing TCP socket.
				if hj, ok := w.(http.Hijacker); ok {
					conn, _, err := hj.Hijack()
					if err == nil {
						conn.Close()
						return
					}
				}
			}
			_, _ = w.Write([]byte(`{"files":[{"id":"f1","name":"00-03.mp4"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	oldAPI, oldTok, oldSleep := driveAPIBase, oauthTokenURL, sleepFunc
	driveAPIBase = srv.URL
	oauthTokenURL = srv.URL + "/token"
	sleepFunc = func(time.Duration) {}
	defer func() {
		driveAPIBase, oauthTokenURL, sleepFunc = oldAPI, oldTok, oldSleep
		invalidateToken()
		folderCache = make(map[string]string)
	}()

	if err := initHTTP(""); err != nil {
		t.Fatal(err)
	}
	invalidateToken()
	folderCache = make(map[string]string)
	cfg := &Config{ClientID: "id", ClientSecret: "sec", RefreshToken: "ref", FolderID: "root"}

	id, err := ensureSubfolder(cfg, "root", "00-03.mp4")
	if err != nil || id != "f1" {
		t.Fatalf("ensureSubfolder after transient drop = (%q, %v), want (f1, nil)", id, err)
	}
	if getAttempts != 2 {
		t.Fatalf("expected 2 GET attempts, got %d", getAttempts)
	}

	exists, err := driveFileExists(cfg, "day1", "00-03.mp4")
	if err != nil || !exists {
		t.Fatalf("driveFileExists = (%v, %v), want (true, nil)", exists, err)
	}
}

func TestFinalCommit403NoZeroByteProbe(t *testing.T) {
	var mu sync.Mutex
	var cl403 []int64
	var cl503 []int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/403":
			cl403 = append(cl403, r.ContentLength)
			if len(cl403) < 3 {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"rateLimitExceeded"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"ok403"}`))
		case "/503":
			cl503 = append(cl503, r.ContentLength)
			if len(cl503) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"ok503"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	oldSleep := sleepFunc
	sleepFunc = func(time.Duration) {}
	defer func() { sleepFunc = oldSleep }()
	if err := initHTTP(""); err != nil {
		t.Fatal(err)
	}

	id, err := uploadStreamResumable(srv.URL+"/403", bytes.NewReader([]byte("hello")))
	if err != nil || id != "ok403" {
		t.Fatalf("403 retry = (%q, %v), want (ok403, nil)", id, err)
	}
	for i, cl := range cl403 {
		if cl <= 0 {
			t.Fatalf("403 attempt %d sent 0-byte probe (ContentLength=%d)", i, cl)
		}
	}

	id, err = uploadStreamResumable(srv.URL+"/503", bytes.NewReader([]byte("hello")))
	if err != nil || id != "ok503" {
		t.Fatalf("503 retry = (%q, %v), want (ok503, nil)", id, err)
	}
	if len(cl503) != 2 || cl503[0] <= 0 || cl503[1] != 0 {
		t.Fatalf("expected 503 to send chunk then 0-byte probe, got ContentLengths=%v", cl503)
	}
}
