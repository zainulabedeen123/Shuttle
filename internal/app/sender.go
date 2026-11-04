package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type outFile struct {
	Abs     string
	Rel     string
	Size    int64
	ModTime int64
}

// Job is an outgoing transfer.
type Job struct {
	ID       string
	PeerID   string
	PeerName string
	Paths    []string
	Files    []*outFile
	Total    int64
	Created  time.Time

	doneBytes atomic.Int64 // bytes of fully committed files
	curBytes  atomic.Int64 // bytes of the current file confirmed sent

	mu       sync.Mutex
	status   string
	errMsg   string
	current  string
	finished time.Time
	cancel   context.CancelFunc

	// sampled by speedLoop under a.mu
	speed     float64
	lastBytes int64
	resync    atomic.Bool // progress jumped (resume); restart speed sampling
}

func (j *Job) sent() int64 { return j.doneBytes.Load() + j.curBytes.Load() }

func (j *Job) set(status, errMsg string) {
	j.mu.Lock()
	j.status, j.errMsg = status, errMsg
	j.mu.Unlock()
}

func (j *Job) isActive() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.finished.IsZero()
}

type progressReader struct {
	r io.Reader
	n *atomic.Int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	k, err := p.r.Read(b)
	p.n.Add(int64(k))
	return k, err
}

// collectFiles expands files and folders into a flat list with relative paths.
func collectFiles(paths []string) ([]*outFile, error) {
	var out []*outFile
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		st, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("cannot read %s: %w", p, err)
		}
		if !st.IsDir() {
			out = append(out, &outFile{Abs: abs, Rel: filepath.Base(abs), Size: st.Size(), ModTime: st.ModTime().Unix()})
			continue
		}
		root := filepath.Base(abs)
		if root == "" || root == "." || root == string(filepath.Separator) || strings.HasSuffix(root, ":") {
			root = "Shared folder"
		}
		err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip unreadable entries
			}
			name := d.Name()
			if d.IsDir() {
				if path != abs && (name == partsDirName || name == "$RECYCLE.BIN" || name == "System Volume Information" || name == ".Trashes") {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() || isJunk(name) {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			rel, err := filepath.Rel(abs, path)
			if err != nil {
				return nil
			}
			out = append(out, &outFile{Abs: path, Rel: root + "/" + filepath.ToSlash(rel), Size: info.Size(), ModTime: info.ModTime().Unix()})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (a *App) startSend(peer Peer, paths []string) (*Job, error) {
	files, err := collectFiles(paths)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errors.New("nothing to send (no files found)")
	}
	// Deterministic ID: re-sending the same files resumes a previous attempt.
	h := sha256.New()
	a.mu.Lock()
	fmt.Fprintf(h, "%s|%s", a.cfg.DeviceID, peer.ID)
	a.mu.Unlock()
	var total int64
	for _, f := range files {
		fmt.Fprintf(h, "|%s|%d|%d", f.Abs, f.Size, f.ModTime)
		total += f.Size
	}
	id := hex.EncodeToString(h.Sum(nil))[:32]

	ctx, cancel := context.WithCancel(context.Background())
	j := &Job{ID: id, PeerID: peer.ID, PeerName: peer.Name, Paths: paths, Files: files, Total: total,
		Created: time.Now(), status: "queued", cancel: cancel}

	a.mu.Lock()
	for i, old := range a.jobs {
		if old.ID == id {
			if old.isActive() {
				a.mu.Unlock()
				cancel()
				return nil, errors.New("these files are already being sent")
			}
			a.jobs = append(a.jobs[:i], a.jobs[i+1:]...)
			break
		}
	}
	a.jobs = append(a.jobs, j)
	a.mu.Unlock()

	go a.runJob(ctx, j)
	return j, nil
}

func (a *App) runJob(ctx context.Context, j *Job) {
	a.busy(1)
	defer a.busy(-1)
	log.Printf("sending %d file(s), %s to %q", len(j.Files), humanBytes(j.Total), j.PeerName)

	err := a.doJob(ctx, j)

	j.mu.Lock()
	j.finished = time.Now()
	switch {
	case err == nil:
		j.status, j.errMsg = "done", ""
	case ctx.Err() != nil:
		j.status, j.errMsg = "cancelled", ""
	default:
		j.status, j.errMsg = "failed", err.Error()
	}
	status := j.status
	j.mu.Unlock()

	switch status {
	case "done":
		var rels []string
		for _, f := range j.Files {
			rels = append(rels, f.Rel)
		}
		a.addHistory(HistoryItem{ID: randomHex(6), Direction: "sent", Peer: j.PeerName, Name: displayName(rels),
			Files: len(j.Files), Bytes: j.Total, Path: j.Paths[0], Time: time.Now(), Seconds: time.Since(j.Created).Seconds()})
		log.Printf("sent %s to %q in %s", humanBytes(j.Total), j.PeerName, time.Since(j.Created).Round(time.Second))
	case "cancelled":
		log.Printf("transfer to %q cancelled", j.PeerName)
		go func() {
			p, ok := a.peerByID(j.PeerID)
			if !ok {
				return
			}
			c := a.newPeerClient(p.Fingerprint, nil, 5*time.Second)
			cctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			_ = peerDo(cctx, c, "POST", a.peerURL(j.PeerID, "/v1/finish/"+j.ID), map[string]string{"status": "cancelled"}, nil)
		}()
	default:
		log.Printf("transfer to %q failed: %v", j.PeerName, err)
	}
}

// retry runs fn until it succeeds, a permanent error occurs, or attempts run out.
func (a *App) retry(ctx context.Context, j *Job, fn func() error) error {
	var err error
	for attempt := 0; attempt < 12; attempt++ {
		if attempt > 0 {
			j.set("reconnecting", err.Error())
			if !sleepCtx(ctx, backoff(attempt)) {
				return ctx.Err()
			}
		}
		if err = fn(); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !retryable(err) {
			return err
		}
	}
	return err
}

func (a *App) doJob(ctx context.Context, j *Job) error {
	peer, ok := a.peerByID(j.PeerID)
	if !ok {
		return errors.New("device is no longer paired")
	}
	client := a.newPeerClient(peer.Fingerprint, nil, 5*time.Minute)
	defer client.CloseIdleConnections()

	offer := &offerReq{TransferID: j.ID, Total: j.Total}
	for _, f := range j.Files {
		offer.Files = append(offer.Files, offerFile{Path: f.Rel, Size: f.Size, ModTime: f.ModTime})
	}
	j.set("connecting", "")
	var res offerResp
	if err := a.retry(ctx, j, func() error {
		return peerDo(ctx, client, "POST", a.peerURL(j.PeerID, "/v1/offer"), offer, &res)
	}); err != nil {
		return err
	}
	if len(res.Offsets) != len(j.Files) || len(res.Done) != len(j.Files) {
		return errors.New("unexpected response from device")
	}

	for i, f := range j.Files {
		if res.Done[i] {
			j.doneBytes.Add(f.Size)
			continue
		}
		j.mu.Lock()
		j.current = f.Rel
		j.mu.Unlock()
		if err := a.sendFile(ctx, client, j, i, res.Offsets[i], offer); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%s: %w", f.Rel, err)
		}
		j.doneBytes.Add(f.Size)
		j.curBytes.Store(0)
	}

	j.set("finishing", "")
	return a.retry(ctx, j, func() error {
		return peerDo(ctx, client, "POST", a.peerURL(j.PeerID, "/v1/finish/"+j.ID), map[string]string{"status": "done"}, nil)
	})
}

// sendFile uploads one file, resuming after network errors and verifying the
// SHA-256 computed by the receiver against our own.
func (a *App) sendFile(ctx context.Context, client *http.Client, j *Job, i int, offset int64, offer *offerReq) error {
	f := j.Files[i]
	failures, mismatches := 0, 0
	var lastErr error
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if failures > 0 {
			if failures > 15 {
				return lastErr
			}
			j.set("reconnecting", lastErr.Error())
			if !sleepCtx(ctx, backoff(failures)) {
				return ctx.Err()
			}
			// Ask the receiver how much it already has.
			var st statusResp
			err := peerDo(ctx, client, "GET", a.peerURL(j.PeerID, fmt.Sprintf("/v1/status/%s/%d", j.ID, i)), nil, &st)
			var he *httpError
			if errors.As(err, &he) && he.Code == 404 { // receiver restarted: re-offer
				var res offerResp
				if err = peerDo(ctx, client, "POST", a.peerURL(j.PeerID, "/v1/offer"), offer, &res); err == nil {
					st = statusResp{Offset: res.Offsets[i], Done: res.Done[i]}
				}
			}
			if err != nil {
				lastErr = err
				if !retryable(err) {
					return err
				}
				failures++
				continue
			}
			if st.Done {
				return nil
			}
			offset = st.Offset
		}

		before := offset
		sum, err := a.uploadOnce(ctx, client, j, i, offset)
		if err == nil {
			var cr commitResp
			err = peerDo(ctx, client, "POST", a.peerURL(j.PeerID, fmt.Sprintf("/v1/commit/%s/%d", j.ID, i)),
				map[string]string{"sha256": sum}, &cr)
			if err == nil {
				return nil
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var he *httpError
		if errors.Is(err, errMismatch) || (errors.As(err, &he) && strings.Contains(he.Msg, "checksum")) {
			mismatches++
			log.Printf("%s: checksum mismatch (attempt %d), restarting file", f.Rel, mismatches)
			if mismatches > 2 {
				return errors.New("data kept arriving corrupted (checksum mismatch)")
			}
			_ = peerDo(ctx, client, "POST", a.peerURL(j.PeerID, fmt.Sprintf("/v1/discard/%s/%d", j.ID, i)), nil, nil)
			offset = 0
			continue
		}
		if !retryable(err) {
			return err
		}
		lastErr = err
		if j.curBytes.Load() > before+(8<<20) {
			failures = 1 // made progress: keep retrying patiently
		} else {
			failures++
		}
		log.Printf("%s: %v — will resume", f.Rel, err)
	}
}

func (a *App) uploadOnce(ctx context.Context, client *http.Client, j *Job, i int, offset int64) (string, error) {
	f := j.Files[i]
	fh, err := os.Open(f.Abs)
	if err != nil {
		return "", permanent(err)
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return "", permanent(err)
	}
	if st.Size() != f.Size {
		return "", permanent(errors.New("file changed while sending; send it again"))
	}
	if offset < 0 || offset > f.Size {
		offset = 0
	}

	h := sha256.New()
	if offset > 0 {
		j.set("resuming", "")
		if _, err := io.CopyBuffer(h, io.LimitReader(fh, offset), make([]byte, 1<<20)); err != nil {
			return "", permanent(err)
		}
	}
	j.curBytes.Store(offset)
	j.resync.Store(true)
	j.set("sending", "")

	remaining := f.Size - offset
	var body io.Reader = http.NoBody
	if remaining > 0 {
		body = &progressReader{r: io.TeeReader(io.LimitReader(fh, remaining), h), n: &j.curBytes}
	}
	req, err := http.NewRequestWithContext(ctx, "PUT",
		a.peerURL(j.PeerID, fmt.Sprintf("/v1/upload/%s/%d?offset=%d", j.ID, i, offset)), body)
	if err != nil {
		return "", err
	}
	req.ContentLength = remaining
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		return "", friendlyNetErr(err)
	}
	defer resp.Body.Close()
	var ur uploadResp
	if err := decodeResp(resp, &ur); err != nil {
		return "", err
	}
	local := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(ur.SHA256, local) {
		return "", errMismatch
	}
	return local, nil
}
