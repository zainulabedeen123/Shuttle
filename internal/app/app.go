// Package app implements Shuttle: LAN file transfer between Windows and macOS.
package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"shuttle/internal/platform"
	"shuttle/internal/window"
)

type Options struct {
	OpenWindow    bool // open the app window once started (dev convenience)
	PeerPort      int
	UIPort        int
	DiscoveryPort int
	Version       string
}

type App struct {
	home    string
	version string

	mu  sync.Mutex
	cfg Config

	cert      tls.Certificate
	fp        string
	token     string
	uiURL     string
	exe       string
	autostart bool

	online   map[string]*onlineDevice
	seen     map[string]time.Time
	pairIn   map[string]*pairRequest
	pairOut  map[string]*pairAttempt
	jobs     []*Job
	incoming map[string]*inTransfer
	history  []HistoryItem

	commitMu  sync.Mutex
	awakeMu   sync.Mutex
	awakeN    int
	uiClients atomic.Int32
	scanning  atomic.Bool
	rescan    chan struct{}
	fwWarning string

	quit     chan struct{}
	quitOnce sync.Once
}

type uiInfo struct {
	URL   string `json:"url"`
	Port  int    `json:"port"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

// HistoryItem is a finished transfer shown in the UI.
type HistoryItem struct {
	ID        string    `json:"id"`
	Direction string    `json:"direction"`
	Peer      string    `json:"peer"`
	Name      string    `json:"name"`
	Files     int       `json:"files"`
	Bytes     int64     `json:"bytes"`
	Path      string    `json:"path"`
	Time      time.Time `json:"time"`
	Seconds   float64   `json:"seconds"`
}

func setupLogging(home string) {
	path := filepath.Join(home, "shuttle.log")
	if st, err := os.Stat(path); err == nil && st.Size() > 5<<20 {
		_ = os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(f, os.Stderr))
}

// Run starts Shuttle in the foreground and blocks until it is told to quit.
func Run(opts Options) error {
	home, err := HomeDir()
	if err != nil {
		return err
	}
	setupLogging(home)
	cfg, err := loadConfig(home)
	if err != nil {
		return err
	}
	if opts.PeerPort > 0 {
		cfg.PeerPort = opts.PeerPort
	}
	if opts.UIPort > 0 {
		cfg.UIPort = opts.UIPort
	}
	if opts.DiscoveryPort > 0 {
		cfg.DiscoveryPort = opts.DiscoveryPort
	}

	// Single instance: if the UI port is taken by a running Shuttle, just show it.
	uiLn, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.UIPort))
	if err != nil {
		if info, e := pingRunning(home); e == nil {
			log.Printf("Shuttle is already running: %s", info.URL)
			return nil
		}
		return fmt.Errorf("cannot listen on 127.0.0.1:%d: %w", cfg.UIPort, err)
	}
	defer uiLn.Close()

	cert, fp, err := loadOrCreateIdentity(home)
	if err != nil {
		return fmt.Errorf("device identity: %w", err)
	}
	exe, _ := os.Executable()
	a := &App{
		home: home, version: opts.Version, cfg: cfg, cert: cert, fp: fp, token: randomHex(16), exe: exe,
		online: map[string]*onlineDevice{}, seen: map[string]time.Time{},
		pairIn: map[string]*pairRequest{}, pairOut: map[string]*pairAttempt{},
		incoming: map[string]*inTransfer{}, quit: make(chan struct{}), rescan: make(chan struct{}, 1),
	}
	a.autostart = platform.AutostartEnabled()
	a.loadHistory()
	_ = os.MkdirAll(cfg.DownloadDir, 0o755)

	if err := a.startPeerServer(); err != nil {
		return err
	}
	a.uiURL = fmt.Sprintf("http://127.0.0.1:%d/", cfg.UIPort)
	infoPath := filepath.Join(home, "ui.json")
	b, _ := json.Marshal(uiInfo{URL: a.uiURL, Port: cfg.UIPort, Token: a.token, PID: os.Getpid()})
	if err := writeFileAtomic(infoPath, b, 0o600); err != nil {
		return err
	}
	defer os.Remove(infoPath)

	if _, err := startDiscovery(a); err != nil {
		log.Printf("auto-discovery disabled (%v) — use Connect by IP", err)
	}
	go a.speedLoop()
	go a.probeLoop()
	go a.scanLoop()
	go a.firewallLoop()
	go a.cleanupParts()
	go func() {
		if err := platform.InstallSendTo(exe); err != nil && !errors.Is(err, platform.ErrUnsupported) {
			log.Printf("Send To menu: %v", err)
		}
	}()

	srv := &http.Server{Handler: a.uiHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(uiLn) }()

	log.Printf("Shuttle %s — device %q, UI %s, peer port %d, IPs %v", a.version, cfg.DeviceName, a.uiURL, cfg.PeerPort, localIPs())
	if opts.OpenWindow {
		go a.launchWindow()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sig:
	case <-a.quit:
	}
	log.Printf("shutting down")
	platform.SetAwake(false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	return nil
}

func (a *App) stop() { a.quitOnce.Do(func() { close(a.quit) }) }

// busy tracks active transfers and keeps the computer awake while any run.
func (a *App) busy(delta int) {
	a.awakeMu.Lock()
	defer a.awakeMu.Unlock()
	prev := a.awakeN
	a.awakeN += delta
	switch {
	case prev == 0 && a.awakeN > 0:
		platform.SetAwake(true)
	case prev > 0 && a.awakeN == 0:
		platform.SetAwake(false)
	}
}

func (a *App) speedLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	ema := func(old float64, delta int64) float64 {
		if old == 0 {
			return float64(delta)
		}
		return old*0.6 + float64(delta)*0.4
	}
	for range t.C {
		a.mu.Lock()
		for _, j := range a.jobs {
			if !j.isActive() {
				j.speed = 0
				continue
			}
			s := j.sent()
			d := s - j.lastBytes
			if d < 0 {
				d = 0
			}
			if j.resync.Swap(false) {
				j.lastBytes = s
				continue
			}
			j.speed, j.lastBytes = ema(j.speed, d), s
		}
		for id, t := range a.incoming {
			r := t.received()
			d := r - t.lastBytes
			if d < 0 {
				d = 0
			}
			if t.resync.Swap(false) {
				t.lastBytes = r
			} else {
				t.speed, t.lastBytes = ema(t.speed, d), r
			}
			// forget transfers whose sender vanished long ago (parts stay for resume)
			if t.uploads.Load() == 0 && time.Since(time.Unix(0, t.lastActivity.Load())) > 30*time.Minute {
				delete(a.incoming, id)
			}
		}
		a.mu.Unlock()
	}
}

func (a *App) loadHistory() {
	b, err := os.ReadFile(filepath.Join(a.home, "history.json"))
	if err == nil {
		_ = json.Unmarshal(b, &a.history)
	}
}

func (a *App) addHistory(h HistoryItem) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.history = append([]HistoryItem{h}, a.history...)
	if len(a.history) > 200 {
		a.history = a.history[:200]
	}
	b, _ := json.MarshalIndent(a.history, "", " ")
	if err := writeFileAtomic(filepath.Join(a.home, "history.json"), b, 0o600); err != nil {
		log.Printf("save history: %v", err)
	}
}

// resolvePeer picks the target device for a send request.
func (a *App) resolvePeer(peerID, to string) (Peer, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	peers := append([]Peer(nil), a.cfg.Peers...)
	if len(peers) == 0 {
		return Peer{}, errors.New("no paired devices yet — open Shuttle and pair your other computer first")
	}
	if peerID != "" {
		for _, p := range peers {
			if p.ID == peerID {
				return p, nil
			}
		}
		return Peer{}, errors.New("device is not paired")
	}
	if to != "" {
		for _, p := range peers {
			if equalFoldTrim(p.Name, to) || p.ID == to {
				return p, nil
			}
		}
		return Peer{}, fmt.Errorf("no paired device named %q", to)
	}
	sort.SliceStable(peers, func(i, j int) bool {
		oi, oj := a.isOnlineLocked(peers[i].ID), a.isOnlineLocked(peers[j].ID)
		if oi != oj {
			return oi
		}
		return a.seen[peers[i].ID].After(a.seen[peers[j].ID])
	})
	return peers[0], nil
}

// ---- helpers used by the CLI to talk to a running instance ----

func pingRunning(home string) (uiInfo, error) {
	var info uiInfo
	b, err := os.ReadFile(filepath.Join(home, "ui.json"))
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(b, &info); err != nil {
		return info, err
	}
	req, _ := http.NewRequest("GET", info.URL+"api/ping", nil)
	req.Header.Set("X-Shuttle-Token", info.Token)
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return info, err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return info, fmt.Errorf("status %d", resp.StatusCode)
	}
	return info, nil
}

// EnsureRunning returns the running instance, starting one in the background if needed.
func EnsureRunning() (uiInfo, error) {
	home, err := HomeDir()
	if err != nil {
		return uiInfo{}, err
	}
	if info, err := pingRunning(home); err == nil {
		return info, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return uiInfo{}, err
	}
	cmd := exec.Command(exe, "run", "--no-browser")
	platform.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return uiInfo{}, err
	}
	_ = cmd.Process.Release()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		if info, err := pingRunning(home); err == nil {
			return info, nil
		}
	}
	return uiInfo{}, errors.New("Shuttle did not start; check shuttle.log in " + home)
}

// OpenWindow makes sure the background service is running, then shows the UI
// in a native app window (blocking until it is closed). Falls back to the
// default browser if no native window is available.
func OpenWindow() error {
	info, err := EnsureRunning()
	if err != nil {
		return err
	}
	home, _ := HomeDir()
	if err := window.Open(info.URL, filepath.Join(home, "webview")); err != nil {
		log.Printf("native window unavailable (%v); using browser", err)
		return platform.OpenURL(info.URL)
	}
	return nil
}

// launchWindow opens the app window from the background service, e.g. when a
// pairing request arrives and no window is open.
func (a *App) launchWindow() {
	if runtime.GOOS == "darwin" {
		if i := strings.Index(a.exe, ".app/Contents/MacOS/"); i > 0 {
			_ = exec.Command("open", a.exe[:i+4]).Start()
			return
		}
	}
	cmd := exec.Command(a.exe, "app")
	platform.Detach(cmd)
	if err := cmd.Start(); err == nil {
		_ = cmd.Process.Release()
	}
}

// SendFromCLI queues files on the running instance (starting it if needed).
func SendFromCLI(to string, paths []string) error {
	info, err := EnsureRunning()
	if err != nil {
		return err
	}
	var abs []string
	for _, p := range paths {
		ap, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		abs = append(abs, ap)
	}
	body, _ := json.Marshal(map[string]any{"to": to, "paths": abs})
	req, _ := http.NewRequest("POST", info.URL+"api/send", bytes.NewReader(body))
	req.Header.Set("X-Shuttle-Token", info.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		Error string `json:"error"`
		Peer  string `json:"peer"`
		Files int    `json:"files"`
		Total int64  `json:"total"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 {
		platform.Notify("Shuttle", "Could not send: "+out.Error)
		return errors.New(out.Error)
	}
	msg := fmt.Sprintf("Sending %d file(s), %s to %s", out.Files, humanBytes(out.Total), out.Peer)
	fmt.Println(msg + " — progress: " + info.URL)
	platform.Notify("Shuttle", msg)
	return nil
}
