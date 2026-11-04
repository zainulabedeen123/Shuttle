package app

import (
	"bytes"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"shuttle/internal/platform"
)

//go:embed web/index.html
var indexHTML string

// pairAttempt is an outgoing pairing request started from our UI.
type pairAttempt struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Code   string `json:"code"`
	Status string `json:"status"` // connecting | waiting | paired | failed
	Error  string `json:"error,omitempty"`
}

func equalFoldTrim(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func (a *App) uiHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.hIndex)
	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) })
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.snapshot()) })
	mux.HandleFunc("GET /api/events", a.hEvents)
	mux.HandleFunc("POST /api/pair", a.hUIPair)
	mux.HandleFunc("POST /api/pair/respond", a.hUIPairRespond)
	mux.HandleFunc("POST /api/pair/dismiss", a.hUIPairDismiss)
	mux.HandleFunc("POST /api/unpair", a.hUIUnpair)
	mux.HandleFunc("POST /api/pick", a.hUIPick)
	mux.HandleFunc("POST /api/send", a.hUISend)
	mux.HandleFunc("POST /api/cancel", a.hUICancel)
	mux.HandleFunc("POST /api/clear", a.hUIClear)
	mux.HandleFunc("POST /api/settings", a.hUISettings)
	mux.HandleFunc("POST /api/reveal", a.hUIReveal)
	mux.HandleFunc("POST /api/rescan", func(w http.ResponseWriter, r *http.Request) {
		a.requestScan()
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]bool{"ok": true})
		go func() { time.Sleep(300 * time.Millisecond); a.stop() }()
	})
	return a.uiGuard(mux)
}

// uiGuard blocks DNS-rebinding (Host check) and cross-site requests (token).
func (a *App) uiGuard(next http.Handler) http.Handler {
	port := strconv.Itoa(a.cfg.UIPort)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "127.0.0.1:"+port && r.Host != "localhost:"+port {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			tok := r.Header.Get("X-Shuttle-Token")
			if tok == "" && r.URL.Path == "/api/events" {
				tok = r.URL.Query().Get("token")
			}
			if subtle.ConstantTimeCompare([]byte(tok), []byte(a.token)) != 1 {
				httpErr(w, http.StatusUnauthorized, "session expired — reload the page")
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func (a *App) hIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(strings.Replace(indexHTML, "__SHUTTLE_TOKEN__", a.token, 1)))
}

// ---------- state snapshot ----------

type meView struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	OS          string   `json:"os"`
	DownloadDir string   `json:"downloadDir"`
	IPs         []string `json:"ips"`
	PeerPort    int      `json:"peerPort"`
	Version     string   `json:"version"`
	Autostart   bool     `json:"autostart"`
	Scanning    bool     `json:"scanning"`
	Firewall    string   `json:"firewall,omitempty"`
	App         bool     `json:"app"`
}

type deviceView struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	OS     string `json:"os"`
	Addr   string `json:"addr"`
	Paired bool   `json:"paired"`
	Online bool   `json:"online"`
}

type pairReqView struct {
	ReqID string `json:"reqId"`
	Name  string `json:"name"`
	OS    string `json:"os"`
	Code  string `json:"code"`
	Addr  string `json:"addr"`
}

type transferView struct {
	ID        string   `json:"id"`
	Direction string   `json:"direction"`
	Peer      string   `json:"peer"`
	PeerID    string   `json:"peerId"`
	Name      string   `json:"name"`
	Files     int      `json:"files"`
	Total     int64    `json:"total"`
	Done      int64    `json:"done"`
	Speed     float64  `json:"speed"`
	Status    string   `json:"status"`
	Error     string   `json:"error,omitempty"`
	Current   string   `json:"current,omitempty"`
	Active    bool     `json:"active"`
	Paths     []string `json:"paths,omitempty"`
	Created   int64    `json:"created"`
}

type stateView struct {
	Me           meView         `json:"me"`
	Devices      []deviceView   `json:"devices"`
	PairRequests []pairReqView  `json:"pairRequests"`
	PairAttempts []pairAttempt  `json:"pairAttempts"`
	Transfers    []transferView `json:"transfers"`
	History      []HistoryItem  `json:"history"`
}

func (a *App) snapshot() stateView {
	ips := localIPs()
	a.mu.Lock()
	defer a.mu.Unlock()
	s := stateView{
		Me: meView{ID: a.cfg.DeviceID, Name: a.cfg.DeviceName, OS: runtime.GOOS, DownloadDir: a.cfg.DownloadDir,
			IPs: ips, PeerPort: a.cfg.PeerPort, Version: a.version, Autostart: a.autostart,
			Scanning: a.scanning.Load(), Firewall: a.fwWarning},
		Devices: []deviceView{}, PairRequests: []pairReqView{}, PairAttempts: []pairAttempt{},
		Transfers: []transferView{}, History: a.history,
	}
	if s.History == nil {
		s.History = []HistoryItem{}
	}
	paired := map[string]bool{}
	for _, p := range a.cfg.Peers {
		paired[p.ID] = true
		s.Devices = append(s.Devices, deviceView{ID: p.ID, Name: p.Name, OS: p.OS, Addr: p.Addr, Paired: true, Online: a.isOnlineLocked(p.ID)})
	}
	for _, d := range a.online {
		if !paired[d.ID] && time.Since(d.LastSeen) < 75*time.Second {
			s.Devices = append(s.Devices, deviceView{ID: d.ID, Name: d.Name, OS: d.OS, Addr: d.Addr, Online: true})
		}
	}
	sort.SliceStable(s.Devices, func(i, j int) bool {
		di, dj := s.Devices[i], s.Devices[j]
		if di.Paired != dj.Paired {
			return di.Paired
		}
		return strings.ToLower(di.Name) < strings.ToLower(dj.Name)
	})
	for _, pr := range a.pairIn {
		s.PairRequests = append(s.PairRequests, pairReqView{ReqID: pr.ReqID, Name: pr.Name, OS: pr.OS, Code: pr.Code, Addr: pr.Addr})
	}
	for _, po := range a.pairOut {
		s.PairAttempts = append(s.PairAttempts, *po)
	}
	sort.Slice(s.PairAttempts, func(i, j int) bool { return s.PairAttempts[i].Key < s.PairAttempts[j].Key })

	for _, t := range a.incoming {
		var rels []string
		for _, f := range t.Files {
			rels = append(rels, f.Rel)
		}
		status := "receiving"
		if t.uploads.Load() == 0 {
			status = "waiting for sender"
		}
		s.Transfers = append(s.Transfers, transferView{ID: t.ID, Direction: "in", Peer: t.PeerName, PeerID: t.PeerID,
			Name: displayName(rels), Files: len(t.Files), Total: t.Total, Done: t.received(), Speed: t.speed,
			Status: status, Active: true, Created: t.Started.UnixMilli()})
	}
	for i := len(a.jobs) - 1; i >= 0; i-- {
		j := a.jobs[i]
		var rels []string
		for _, f := range j.Files {
			rels = append(rels, f.Rel)
		}
		j.mu.Lock()
		v := transferView{ID: j.ID, Direction: "out", Peer: j.PeerName, PeerID: j.PeerID, Name: displayName(rels),
			Files: len(j.Files), Total: j.Total, Done: j.sent(), Speed: j.speed, Status: j.status, Error: j.errMsg,
			Current: j.current, Active: j.finished.IsZero(), Paths: j.Paths, Created: j.Created.UnixMilli()}
		j.mu.Unlock()
		if v.Status == "done" {
			v.Done = v.Total
		}
		s.Transfers = append(s.Transfers, v)
	}
	sort.SliceStable(s.Transfers, func(i, j int) bool {
		if s.Transfers[i].Active != s.Transfers[j].Active {
			return s.Transfers[i].Active
		}
		return s.Transfers[i].Created > s.Transfers[j].Created
	})
	return s
}

func (a *App) hEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		httpErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	a.uiClients.Add(1)
	defer a.uiClients.Add(-1)
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	var last []byte
	idle := 0
	for {
		b, _ := json.Marshal(a.snapshot())
		if !bytes.Equal(b, last) {
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
			last, idle = b, 0
		} else if idle++; idle > 30 {
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
			idle = 0
		}
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
		}
	}
}

// ---------- pairing ----------

func (a *App) hUIPair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceID string `json:"deviceId"`
		Addr     string `json:"addr"`
	}
	if err := readJSON(r, &req); err != nil {
		httpErr(w, 400, "bad request")
		return
	}
	var addr string
	if req.DeviceID != "" {
		a.mu.Lock()
		if d := a.online[req.DeviceID]; d != nil {
			addr = d.Addr
		}
		a.mu.Unlock()
		if addr == "" {
			httpErr(w, 404, "device is no longer visible")
			return
		}
	} else {
		var err error
		if addr, err = normalizeAddr(req.Addr); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
	}
	a.mu.Lock()
	if po := a.pairOut[addr]; po != nil && (po.Status == "connecting" || po.Status == "waiting") {
		a.mu.Unlock()
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	a.pairOut[addr] = &pairAttempt{Key: addr, Name: addr, Status: "connecting"}
	a.mu.Unlock()
	go a.pairWith(addr)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) setAttempt(key string, f func(*pairAttempt)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if po := a.pairOut[key]; po != nil {
		f(po)
	}
}

func (a *App) pairWith(addr string) {
	fail := func(err error) {
		msg := err.Error()
		if strings.Contains(msg, "timed out") || strings.Contains(msg, "not reachable") || strings.Contains(msg, "no route") || strings.Contains(msg, "unreachable") {
			msg = "no answer. Check that Shuttle is open on that computer, that the IP matches the one shown at the top of its Shuttle window, and that its firewall allows Shuttle"
		}
		a.setAttempt(addr, func(p *pairAttempt) { p.Status, p.Error = "failed", msg })
		log.Printf("pairing with %s failed: %v", addr, err)
	}
	ctx := context.Background()
	var cap capturedFP
	c := a.newPeerClient("", &cap, 10*time.Second)
	defer c.CloseIdleConnections()
	var hello helloResp
	if err := peerDo(ctx, c, "GET", "https://"+addr+"/v1/hello", nil, &hello); err != nil {
		fail(err)
		return
	}
	fp := cap.get()
	a.mu.Lock()
	myID, myName, myPort := a.cfg.DeviceID, a.cfg.DeviceName, a.cfg.PeerPort
	a.mu.Unlock()
	if hello.ID == myID {
		fail(errors.New("that address is this device"))
		return
	}
	code := pairCode(a.fp, fp)
	a.setAttempt(addr, func(p *pairAttempt) { p.Name, p.Code, p.Status = hello.Name, code, "waiting" })

	c2 := a.newPeerClient(fp, nil, 150*time.Second)
	defer c2.CloseIdleConnections()
	var res pairResp
	err := peerDo(ctx, c2, "POST", "https://"+addr+"/v1/pair", pairReq{ID: myID, Name: myName, OS: runtime.GOOS, Port: myPort}, &res)
	if err != nil {
		fail(err)
		return
	}
	if !res.Accepted {
		reason := res.Reason
		if reason == "" {
			reason = "declined"
		}
		fail(errors.New("pairing " + reason))
		return
	}
	a.addPeer(Peer{ID: res.ID, Name: res.Name, OS: res.OS, Fingerprint: fp, Addr: addr, PairedAt: time.Now()})
	a.setAttempt(addr, func(p *pairAttempt) { p.Status, p.Error = "paired", "" })
	log.Printf("paired with %q at %s", res.Name, addr)
	time.Sleep(4 * time.Second)
	a.mu.Lock()
	delete(a.pairOut, addr)
	a.mu.Unlock()
}

func (a *App) hUIPairRespond(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ReqID  string `json:"reqId"`
		Accept bool   `json:"accept"`
	}
	if err := readJSON(r, &req); err != nil {
		httpErr(w, 400, "bad request")
		return
	}
	a.mu.Lock()
	pr := a.pairIn[req.ReqID]
	a.mu.Unlock()
	if pr == nil {
		httpErr(w, 404, "pairing request expired")
		return
	}
	select {
	case pr.decision <- req.Accept:
	default:
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) hUIPairDismiss(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	_ = readJSON(r, &req)
	a.mu.Lock()
	delete(a.pairOut, req.Key)
	a.mu.Unlock()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) hUIUnpair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PeerID string `json:"peerId"`
	}
	_ = readJSON(r, &req)
	p, ok := a.peerByID(req.PeerID)
	if !ok {
		httpErr(w, 404, "not paired")
		return
	}
	go func() { // tell the other side, best effort
		c := a.newPeerClient(p.Fingerprint, nil, 4*time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = peerDo(ctx, c, "POST", "https://"+p.Addr+"/v1/unpair", map[string]bool{}, nil)
	}()
	a.removePeer(p.ID)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- sending ----------

func (a *App) hUIPick(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind string `json:"kind"`
	}
	_ = readJSON(r, &req)
	var paths []string
	var err error
	switch req.Kind {
	case "folders":
		paths, err = platform.PickFolders()
	case "dir":
		var p string
		p, err = platform.PickFolder("Choose where received files are saved")
		if p != "" {
			paths = []string{p}
		}
	default:
		paths, err = platform.PickFiles()
	}
	if err != nil {
		httpErr(w, 500, "file dialog failed: "+err.Error())
		return
	}
	if paths == nil {
		paths = []string{}
	}
	writeJSON(w, 200, map[string]any{"paths": paths})
}

func (a *App) hUISend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PeerID string   `json:"peerId"`
		To     string   `json:"to"`
		Paths  []string `json:"paths"`
	}
	if err := readJSON(r, &req); err != nil || len(req.Paths) == 0 {
		httpErr(w, 400, "nothing to send")
		return
	}
	peer, err := a.resolvePeer(req.PeerID, req.To)
	if err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	j, err := a.startSend(peer, req.Paths)
	if err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": j.ID, "peer": peer.Name, "files": len(j.Files), "total": j.Total})
}

func (a *App) hUICancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	_ = readJSON(r, &req)
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, j := range a.jobs {
		if j.ID == req.ID {
			j.cancel()
		}
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) hUIClear(w http.ResponseWriter, r *http.Request) {
	var req struct {
		History bool `json:"history"`
	}
	_ = readJSON(r, &req)
	a.mu.Lock()
	out := a.jobs[:0]
	for _, j := range a.jobs {
		if j.isActive() {
			out = append(out, j)
		}
	}
	a.jobs = out
	if req.History {
		a.history = nil
		_ = os.Remove(filepath.Join(a.home, "history.json"))
	}
	a.mu.Unlock()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- settings & misc ----------

func (a *App) hUISettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceName  *string `json:"deviceName"`
		DownloadDir *string `json:"downloadDir"`
		Autostart   *bool   `json:"autostart"`
	}
	if err := readJSON(r, &req); err != nil {
		httpErr(w, 400, "bad request")
		return
	}
	if req.DownloadDir != nil {
		dir := filepath.Clean(strings.TrimSpace(*req.DownloadDir))
		if !filepath.IsAbs(dir) {
			httpErr(w, 400, "download folder must be a full path")
			return
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			httpErr(w, 400, "cannot use that folder: "+err.Error())
			return
		}
		probe := filepath.Join(dir, ".shuttle-write-test")
		if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
			httpErr(w, 400, "folder is not writable: "+err.Error())
			return
		}
		_ = os.Remove(probe)
		a.mu.Lock()
		a.cfg.DownloadDir = dir
		a.saveConfigLocked()
		a.mu.Unlock()
	}
	if req.DeviceName != nil {
		name := strings.TrimSpace(*req.DeviceName)
		if name == "" || len(name) > 64 {
			httpErr(w, 400, "device name must be 1–64 characters")
			return
		}
		a.mu.Lock()
		a.cfg.DeviceName = name
		a.saveConfigLocked()
		a.mu.Unlock()
	}
	if req.Autostart != nil {
		if err := platform.SetAutostart(*req.Autostart, a.exe); err != nil {
			httpErr(w, 500, "start at login: "+err.Error())
			return
		}
		a.mu.Lock()
		a.autostart = *req.Autostart
		a.mu.Unlock()
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) hUIReveal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID        string `json:"id"`
		Downloads bool   `json:"downloads"`
	}
	_ = readJSON(r, &req)
	if req.Downloads {
		dir := a.downloadDir()
		_ = os.MkdirAll(dir, 0o755)
		_ = platform.OpenFolder(dir)
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	a.mu.Lock()
	var path string
	for _, h := range a.history {
		if h.ID == req.ID {
			path = h.Path
		}
	}
	a.mu.Unlock()
	if path == "" {
		httpErr(w, 404, "not found")
		return
	}
	if _, err := os.Stat(path); err != nil {
		httpErr(w, 404, "the file is no longer there")
		return
	}
	_ = platform.Reveal(path)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
