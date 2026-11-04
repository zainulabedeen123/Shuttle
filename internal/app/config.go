package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"shuttle/internal/platform"
)

const (
	defaultPeerPort      = 47820
	defaultUIPort        = 47821
	defaultDiscoveryPort = 47822
)

// Peer is a paired (trusted) device.
type Peer struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	OS          string    `json:"os"`
	Fingerprint string    `json:"fingerprint"`
	Addr        string    `json:"addr"` // last known host:port
	PairedAt    time.Time `json:"pairedAt"`
}

type Config struct {
	DeviceID      string `json:"deviceId"`
	DeviceName    string `json:"deviceName"`
	DownloadDir   string `json:"downloadDir"`
	PeerPort      int    `json:"peerPort"`
	UIPort        int    `json:"uiPort"`
	DiscoveryPort int    `json:"discoveryPort"`
	Peers         []Peer `json:"peers"`
}

// HomeDir returns Shuttle's config directory, creating it if needed.
// Override with the SHUTTLE_HOME environment variable.
func HomeDir() (string, error) {
	if h := os.Getenv("SHUTTLE_HOME"); h != "" {
		return h, os.MkdirAll(h, 0o700)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	h := filepath.Join(base, "Shuttle")
	return h, os.MkdirAll(h, 0o700)
}

func loadConfig(home string) (Config, error) {
	var cfg Config
	path := filepath.Join(home, "config.json")
	b, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return cfg, readErr
	}
	changed := readErr != nil
	if readErr == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			return cfg, fmt.Errorf("%s is corrupt: %w", path, err)
		}
	}
	if cfg.DeviceID == "" {
		cfg.DeviceID = randomHex(8)
		changed = true
	}
	if cfg.DeviceName == "" {
		cfg.DeviceName = platform.ComputerName()
		changed = true
	}
	if cfg.DownloadDir == "" {
		h, _ := os.UserHomeDir()
		cfg.DownloadDir = filepath.Join(h, "Downloads", "Shuttle")
		changed = true
	}
	if cfg.PeerPort == 0 {
		cfg.PeerPort = defaultPeerPort
		changed = true
	}
	if cfg.UIPort == 0 {
		cfg.UIPort = defaultUIPort
		changed = true
	}
	if cfg.DiscoveryPort == 0 {
		cfg.DiscoveryPort = defaultDiscoveryPort
		changed = true
	}
	if changed {
		if err := saveConfig(home, cfg); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

func saveConfig(home string, cfg Config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(home, "config.json"), b, 0o600)
}

// saveConfigLocked persists a.cfg; caller must hold a.mu.
func (a *App) saveConfigLocked() {
	if err := saveConfig(a.home, a.cfg); err != nil {
		log.Printf("save config: %v", err)
	}
}

func (a *App) peerByID(id string) (Peer, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range a.cfg.Peers {
		if p.ID == id {
			return p, true
		}
	}
	return Peer{}, false
}

func (a *App) peerByFP(fp string) (Peer, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range a.cfg.Peers {
		if p.Fingerprint == fp {
			return p, true
		}
	}
	return Peer{}, false
}

// addPeer stores or replaces a paired device (matched by ID or fingerprint).
func (a *App) addPeer(p Peer) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.cfg.Peers[:0]
	for _, old := range a.cfg.Peers {
		if old.ID != p.ID && old.Fingerprint != p.Fingerprint {
			out = append(out, old)
		}
	}
	a.cfg.Peers = append(out, p)
	a.seen[p.ID] = time.Now()
	a.saveConfigLocked()
}

func (a *App) removePeer(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.cfg.Peers[:0]
	for _, old := range a.cfg.Peers {
		if old.ID != id {
			out = append(out, old)
		}
	}
	a.cfg.Peers = out
	a.saveConfigLocked()
}

func (a *App) downloadDir() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.DownloadDir
}
