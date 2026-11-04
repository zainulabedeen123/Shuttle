package app

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"runtime"
	"strconv"
	"time"
)

type announcement struct {
	App   string `json:"app"`
	V     int    `json:"v"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	OS    string `json:"os"`
	Port  int    `json:"port"`
	FP    string `json:"fp"`
	Reply bool   `json:"reply,omitempty"`
}

type onlineDevice struct {
	ID       string
	Name     string
	OS       string
	FP       string
	Addr     string
	LastSeen time.Time
}

type discovery struct {
	a    *App
	conn *net.UDPConn
	port int
}

func startDiscovery(a *App) (*discovery, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(os.Getenv("SHUTTLE_BIND")), Port: a.cfg.DiscoveryPort})
	if err != nil {
		return nil, err
	}
	d := &discovery{a: a, conn: conn, port: a.cfg.DiscoveryPort}
	go d.readLoop()
	go d.announceLoop()
	return d, nil
}

func (d *discovery) msg(reply bool) []byte {
	d.a.mu.Lock()
	m := announcement{App: "shuttle", V: 1, ID: d.a.cfg.DeviceID, Name: d.a.cfg.DeviceName, OS: runtime.GOOS,
		Port: d.a.cfg.PeerPort, FP: d.a.fp, Reply: reply}
	d.a.mu.Unlock()
	b, _ := json.Marshal(m)
	return b
}

func (d *discovery) announceLoop() {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		d.broadcast()
		<-t.C
	}
}

func (d *discovery) broadcast() {
	b := d.msg(false)
	targets := append(broadcastAddrs(), net.IPv4bcast)
	for _, ip := range targets {
		_, _ = d.conn.WriteToUDP(b, &net.UDPAddr{IP: ip, Port: d.port})
	}
}

func (d *discovery) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, src, err := d.conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue // e.g. Windows ICMP port-unreachable resets
		}
		var m announcement
		if json.Unmarshal(buf[:n], &m) != nil || m.App != "shuttle" || m.ID == "" || m.Port <= 0 || m.Port > 65535 {
			continue
		}
		if m.ID == d.a.cfg.DeviceID {
			continue
		}
		addr := net.JoinHostPort(src.IP.String(), strconv.Itoa(m.Port))
		d.a.deviceSeen(m, addr)
		if !m.Reply {
			_, _ = d.conn.WriteToUDP(d.msg(true), src)
		}
	}
}

func (a *App) deviceSeen(m announcement, addr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.online[m.ID] = &onlineDevice{ID: m.ID, Name: m.Name, OS: m.OS, FP: m.FP, Addr: addr, LastSeen: time.Now()}
	changed := false
	for i := range a.cfg.Peers {
		p := &a.cfg.Peers[i]
		if p.ID == m.ID && p.Fingerprint == m.FP {
			if p.Addr != addr {
				p.Addr, changed = addr, true
			}
			if m.Name != "" && p.Name != m.Name {
				p.Name, changed = m.Name, true
			}
			if m.OS != "" && p.OS != m.OS {
				p.OS, changed = m.OS, true
			}
		}
	}
	if changed {
		a.saveConfigLocked()
	}
}

func (a *App) isOnlineLocked(id string) bool {
	if d := a.online[id]; d != nil && time.Since(d.LastSeen) < 10*time.Second {
		return true
	}
	return time.Since(a.seen[id]) < 30*time.Second
}

// probeLoop pings paired devices that discovery can't see (e.g. broadcast-blocking
// networks) at their last known address so their status stays accurate.
func (a *App) probeLoop() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		a.mu.Lock()
		var targets []Peer
		for _, p := range a.cfg.Peers {
			if !a.isOnlineLocked(p.ID) && p.Addr != "" {
				targets = append(targets, p)
			}
		}
		a.mu.Unlock()
		for _, p := range targets {
			go func(p Peer) {
				c := a.newPeerClient(p.Fingerprint, nil, 4*time.Second)
				defer c.CloseIdleConnections()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				var h helloResp
				if peerDo(ctx, c, "GET", "https://"+p.Addr+"/v1/hello", nil, &h) == nil && h.ID == p.ID {
					a.mu.Lock()
					a.seen[p.ID] = time.Now()
					a.mu.Unlock()
				}
			}(p)
		}
		<-t.C
	}
}
