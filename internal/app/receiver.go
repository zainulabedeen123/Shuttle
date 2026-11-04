package app

import (
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"shuttle/internal/platform"
)

const partsDirName = ".shuttle-parts"

type offerFile struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"modTime"`
}

type offerReq struct {
	TransferID string      `json:"transferId"`
	Files      []offerFile `json:"files"`
	Total      int64       `json:"total"`
}

type offerResp struct {
	Offsets []int64 `json:"offsets"`
	Done    []bool  `json:"done"`
}

type statusResp struct {
	Offset int64 `json:"offset"`
	Done   bool  `json:"done"`
}

type uploadResp struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type commitResp struct {
	SavedAs string `json:"savedAs"`
}

type inFile struct {
	Rel     string
	Size    int64
	ModTime int64
	part    string

	mu      sync.Mutex // held while uploading / committing
	have    atomic.Int64
	hash    string
	done    bool
	savedAs string

	// SHA-256 state saved when an upload is interrupted, so a resume at the
	// same offset doesn't have to re-read gigabytes of partial data.
	ckpt    []byte
	ckptOff int64
}

type inTransfer struct {
	ID       string
	PeerID   string
	PeerName string
	Files    []*inFile
	Total    int64
	Started  time.Time

	uploads      atomic.Int32
	lastActivity atomic.Int64 // unix nanos

	// sampled by speedLoop under a.mu
	speed     float64
	lastBytes int64
	resync    atomic.Bool // progress jumped (resume); restart speed sampling
}

func (t *inTransfer) received() int64 {
	var n int64
	for _, f := range t.Files {
		n += f.have.Load()
	}
	return n
}

func (t *inTransfer) touch() { t.lastActivity.Store(time.Now().UnixNano()) }

func (a *App) lookupIn(w http.ResponseWriter, r *http.Request, p Peer) (*inTransfer, *inFile, bool) {
	tid := r.PathValue("tid")
	idx, err := strconv.Atoi(r.PathValue("idx"))
	a.mu.Lock()
	t := a.incoming[tid]
	a.mu.Unlock()
	if t == nil || t.PeerID != p.ID {
		httpErr(w, 404, "unknown transfer")
		return nil, nil, false
	}
	if err != nil || idx < 0 || idx >= len(t.Files) {
		httpErr(w, 400, "bad file index")
		return nil, nil, false
	}
	return t, t.Files[idx], true
}

func (a *App) hOffer(w http.ResponseWriter, r *http.Request, p Peer) {
	var req offerReq
	if err := readJSON(r, &req); err != nil || !tidPattern.MatchString(req.TransferID) || len(req.Files) == 0 || len(req.Files) > 200000 {
		httpErr(w, 400, "bad offer")
		return
	}
	dl := a.downloadDir()
	parts := filepath.Join(dl, partsDirName)
	if err := os.MkdirAll(parts, 0o755); err != nil {
		httpErr(w, 500, "cannot create download folder: "+err.Error())
		return
	}
	platform.HideFile(parts)

	a.mu.Lock()
	t := a.incoming[req.TransferID]
	if t == nil || t.PeerID != p.ID || len(t.Files) != len(req.Files) {
		t = &inTransfer{ID: req.TransferID, PeerID: p.ID, PeerName: p.Name, Started: time.Now()}
		for i, f := range req.Files {
			rel, err := sanitizeRelPath(f.Path)
			if err != nil || f.Size < 0 {
				a.mu.Unlock()
				httpErr(w, 400, "bad file entry: "+f.Path)
				return
			}
			t.Files = append(t.Files, &inFile{
				Rel: rel, Size: f.Size, ModTime: f.ModTime,
				part: filepath.Join(parts, fmt.Sprintf("%s-%d.part", req.TransferID, i)),
			})
			t.Total += f.Size
		}
		a.incoming[req.TransferID] = t
	}
	a.mu.Unlock()
	t.touch()

	resp := offerResp{Offsets: make([]int64, len(t.Files)), Done: make([]bool, len(t.Files))}
	var need int64
	for i, f := range t.Files {
		f.mu.Lock()
		if f.done {
			resp.Done[i], resp.Offsets[i] = true, f.Size
		} else {
			var off int64
			if st, err := os.Stat(f.part); err == nil {
				off = st.Size()
				if off > f.Size {
					_ = os.Truncate(f.part, 0)
					off = 0
				}
			}
			resp.Offsets[i] = off
			f.have.Store(off)
			t.resync.Store(true)
			need += f.Size - off
		}
		f.mu.Unlock()
	}
	if free := platform.FreeSpace(dl); free >= 0 && need > free-(256<<20) {
		httpErr(w, http.StatusInsufficientStorage, fmt.Sprintf("not enough disk space on the receiving computer: needs %s, %s free",
			humanBytes(need), humanBytes(free)))
		return
	}
	log.Printf("incoming from %q: %d file(s), %s (resume %s)", p.Name, len(t.Files), humanBytes(t.Total), humanBytes(t.Total-need))
	writeJSON(w, 200, resp)
}

func (a *App) hStatus(w http.ResponseWriter, r *http.Request, p Peer) {
	_, f, ok := a.lookupIn(w, r, p)
	if !ok {
		return
	}
	if !f.mu.TryLock() {
		httpErr(w, 409, "upload still in progress")
		return
	}
	defer f.mu.Unlock()
	var off int64
	if st, err := os.Stat(f.part); err == nil {
		off = st.Size()
	}
	if f.done {
		off = f.Size
	}
	writeJSON(w, 200, statusResp{Offset: off, Done: f.done})
}

// stallReader extends the connection read deadline on every read so a dead
// sender is detected within a minute instead of blocking forever.
type stallReader struct {
	r  io.Reader
	rc *http.ResponseController
	d  time.Duration
}

func (s *stallReader) Read(p []byte) (int, error) {
	_ = s.rc.SetReadDeadline(time.Now().Add(s.d))
	return s.r.Read(p)
}

func (a *App) hUpload(w http.ResponseWriter, r *http.Request, p Peer) {
	t, f, ok := a.lookupIn(w, r, p)
	if !ok {
		return
	}
	if !f.mu.TryLock() {
		httpErr(w, 409, "file is already being uploaded")
		return
	}
	defer f.mu.Unlock()
	if f.done {
		httpErr(w, 409, "file already committed")
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 || offset > f.Size {
		httpErr(w, 400, "bad offset")
		return
	}
	remaining := f.Size - offset
	if r.ContentLength != remaining {
		httpErr(w, 400, "content length does not match offset")
		return
	}

	fh, err := os.OpenFile(f.part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	if offset > st.Size() {
		cur := st.Size()
		writeJSON(w, 409, map[string]any{"error": "offset beyond received data", "offset": cur})
		return
	}
	if offset < st.Size() {
		if err := fh.Truncate(offset); err != nil {
			httpErr(w, 500, err.Error())
			return
		}
	}

	a.busy(1)
	defer a.busy(-1)
	t.uploads.Add(1)
	defer t.uploads.Add(-1)
	t.touch()

	buf := make([]byte, 1<<20)
	h := sha256.New()
	restored := false
	if offset > 0 && f.ckpt != nil && f.ckptOff == offset {
		restored = h.(encoding.BinaryUnmarshaler).UnmarshalBinary(f.ckpt) == nil
		if !restored {
			h.Reset()
		}
	}
	f.ckpt = nil
	if offset > 0 && !restored {
		if _, err := io.CopyBuffer(h, io.LimitReader(fh, offset), buf); err != nil {
			httpErr(w, 500, "re-reading partial file: "+err.Error())
			return
		}
	}
	if _, err := fh.Seek(offset, io.SeekStart); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	f.have.Store(offset)
	t.resync.Store(true)
	f.hash = ""

	rc := http.NewResponseController(w)
	body := &stallReader{r: r.Body, rc: rc, d: 60 * time.Second}
	n, err := copyChunked(fh, h, body, remaining, buf, &f.have, t)
	_ = rc.SetReadDeadline(time.Time{})
	if err != nil || n != remaining {
		log.Printf("upload of %q interrupted at %s: %v", f.Rel, humanBytes(offset+n), err)
		if st, mErr := h.(encoding.BinaryMarshaler).MarshalBinary(); mErr == nil {
			f.ckpt, f.ckptOff = st, offset+n
		}
		return // partial data stays on disk for resume
	}
	if err := fh.Sync(); err != nil {
		httpErr(w, 500, "flush to disk failed: "+err.Error())
		return
	}
	f.hash = hex.EncodeToString(h.Sum(nil))
	writeJSON(w, 200, uploadResp{Size: f.Size, SHA256: f.hash})
}

// copyChunked copies exactly n bytes from src into file and hash using full-size
// chunks (TLS hands us ~16 KB reads; batching them keeps disk writes efficient).
func copyChunked(dst *os.File, h hash.Hash, src io.Reader, n int64, buf []byte, have *atomic.Int64, t *inTransfer) (int64, error) {
	var done int64
	for done < n {
		want := int64(len(buf))
		if n-done < want {
			want = n - done
		}
		k, err := io.ReadFull(src, buf[:want])
		if k > 0 {
			if _, werr := dst.Write(buf[:k]); werr != nil {
				return done, werr
			}
			h.Write(buf[:k])
			done += int64(k)
			have.Add(int64(k))
			t.touch()
		}
		if err != nil {
			return done, err
		}
	}
	return done, nil
}

func (a *App) hCommit(w http.ResponseWriter, r *http.Request, p Peer) {
	t, f, ok := a.lookupIn(w, r, p)
	if !ok {
		return
	}
	var req struct {
		SHA256 string `json:"sha256"`
	}
	if err := readJSON(r, &req); err != nil {
		httpErr(w, 400, "bad commit")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done {
		writeJSON(w, 200, commitResp{SavedAs: f.savedAs})
		return
	}
	st, err := os.Stat(f.part)
	if err != nil || st.Size() != f.Size {
		httpErr(w, 409, "file incomplete")
		return
	}
	if f.hash == "" {
		sum, err := hashFile(f.part)
		if err != nil {
			httpErr(w, 500, err.Error())
			return
		}
		f.hash = sum
	}
	if !strings.EqualFold(f.hash, req.SHA256) {
		httpErr(w, 409, "checksum mismatch")
		return
	}

	a.commitMu.Lock()
	final := uniquePath(filepath.Join(a.downloadDir(), filepath.FromSlash(f.Rel)))
	err = os.MkdirAll(filepath.Dir(final), 0o755)
	if err == nil {
		err = os.Rename(f.part, final)
	}
	a.commitMu.Unlock()
	if err != nil {
		httpErr(w, 500, "saving file: "+err.Error())
		return
	}
	if f.ModTime > 0 {
		mt := time.Unix(f.ModTime, 0)
		_ = os.Chtimes(final, mt, mt)
	}
	f.done, f.savedAs = true, final
	f.have.Store(f.Size)
	t.touch()
	writeJSON(w, 200, commitResp{SavedAs: final})
}

func (a *App) hDiscard(w http.ResponseWriter, r *http.Request, p Peer) {
	_, f, ok := a.lookupIn(w, r, p)
	if !ok {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.done {
		_ = os.Remove(f.part)
		f.hash, f.ckpt = "", nil
		f.have.Store(0)
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) hFinish(w http.ResponseWriter, r *http.Request, p Peer) {
	tid := r.PathValue("tid")
	var req struct {
		Status string `json:"status"`
	}
	_ = readJSON(r, &req)
	a.mu.Lock()
	t := a.incoming[tid]
	if t != nil && t.PeerID == p.ID {
		delete(a.incoming, tid)
	}
	a.mu.Unlock()
	if t == nil || t.PeerID != p.ID {
		writeJSON(w, 200, map[string]bool{"ok": true}) // idempotent
		return
	}
	if req.Status != "done" {
		log.Printf("transfer from %q cancelled by sender", p.Name)
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	var rels []string
	first := ""
	for _, f := range t.Files {
		rels = append(rels, f.Rel)
		if first == "" && f.savedAs != "" {
			first = f.savedAs
		}
	}
	name := displayName(rels)
	a.addHistory(HistoryItem{
		ID: randomHex(6), Direction: "received", Peer: p.Name, Name: name, Files: len(t.Files),
		Bytes: t.Total, Path: first, Time: time.Now(), Seconds: time.Since(t.Started).Seconds(),
	})
	log.Printf("received %q (%s) from %q", name, humanBytes(t.Total), p.Name)
	a.attention("File received", fmt.Sprintf("%s from %s", name, p.Name))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, 1<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// cleanupParts removes abandoned partial files older than 7 days.
func (a *App) cleanupParts() {
	dir := filepath.Join(a.downloadDir(), partsDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err == nil && time.Since(info.ModTime()) > 7*24*time.Hour {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Printf("cleanup: %v", err)
			}
		}
	}
}
