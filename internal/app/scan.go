package app

import (
	"context"
	"encoding/binary"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"shuttle/internal/platform"
)

// scanLoop actively looks for Shuttle on every address of the local subnet(s).
// UDP broadcast discovery is fast but often blocked (router settings, macOS
// Local Network privacy, guest Wi-Fi); a TCP sweep of a /24 takes ~2 s and
// finds devices regardless.
func (a *App) scanLoop() {
	for {
		a.scanOnce()
		interval := 2 * time.Minute
		if a.uiClients.Load() > 0 {
			interval = 30 * time.Second
		}
		select {
		case <-a.rescan:
		case <-time.After(interval):
		}
	}
}

func (a *App) requestScan() {
	select {
	case a.rescan <- struct{}{}:
	default:
	}
}

// scanTargets lists every host address on our local IPv4 subnets (including
// our own address, where another Shuttle may run on a different port).
func scanTargets() []net.IP {
	var out []net.IP
	seen := map[uint32]bool{}
	for _, n := range usableIPv4Nets() {
		m := n.Mask
		if len(m) == 16 {
			m = m[12:]
		}
		ones, bits := m.Size()
		if bits != 32 {
			continue
		}
		if ones < 22 { // huge networks: just scan our own /24
			ones = 24
		}
		if ones > 30 {
			continue
		}
		base := binary.BigEndian.Uint32(n.IP.To4()) &^ (1<<(32-ones) - 1)
		size := uint32(1) << (32 - ones)
		for i := uint32(1); i < size-1; i++ {
			v := base + i
			if seen[v] {
				continue
			}
			seen[v] = true
			ip := make(net.IP, 4)
			binary.BigEndian.PutUint32(ip, v)
			out = append(out, ip)
		}
	}
	return out
}

func (a *App) scanOnce() {
	a.scanning.Store(true)
	defer a.scanning.Store(false)
	start := time.Now()

	a.mu.Lock()
	ownPort := a.cfg.PeerPort
	a.mu.Unlock()
	ports := map[int]bool{defaultPeerPort: true, ownPort: true}
	self := map[string]bool{}
	for _, ip := range localIPs() {
		self[ip] = true
	}

	sem := make(chan struct{}, 128)
	var wg sync.WaitGroup
	var mu sync.Mutex
	found := 0
	for _, ip := range scanTargets() {
		for port := range ports {
			if port == ownPort && self[ip.String()] {
				continue // that's us
			}
			addr := net.JoinHostPort(ip.String(), strconv.Itoa(port))
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				c, err := net.DialTimeout("tcp", addr, 700*time.Millisecond)
				if err != nil {
					return
				}
				c.Close()
				if a.probeDevice(addr) {
					mu.Lock()
					found++
					mu.Unlock()
				}
			}()
		}
	}
	wg.Wait()
	if found > 0 {
		log.Printf("network scan: %d device(s) found in %s", found, time.Since(start).Round(time.Millisecond))
	}
}

// probeDevice asks a Shuttle instance at addr who it is and records it.
func (a *App) probeDevice(addr string) bool {
	var cap capturedFP
	c := a.newPeerClient("", &cap, 4*time.Second)
	defer c.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var h helloResp
	if err := peerDo(ctx, c, "GET", "https://"+addr+"/v1/hello", nil, &h); err != nil || h.ID == "" {
		return false
	}
	if h.ID == a.cfg.DeviceID {
		return false
	}
	_, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	a.deviceSeen(announcement{App: "shuttle", V: 1, ID: h.ID, Name: h.Name, OS: h.OS, Port: port, FP: cap.get()}, addr)
	return true
}

// firewallLoop periodically checks whether the OS firewall blocks Shuttle.
func (a *App) firewallLoop() {
	for {
		w := platform.FirewallWarning(a.exe)
		a.mu.Lock()
		a.fwWarning = w
		a.mu.Unlock()
		if w != "" {
			log.Printf("firewall: %s", w)
		}
		time.Sleep(3 * time.Minute)
	}
}
