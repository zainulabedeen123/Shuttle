package app

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"time"

	"shuttle/internal/platform"
)

type helloResp struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	OS      string `json:"os"`
	Version string `json:"version"`
	Paired  bool   `json:"paired"`
}

type pairReq struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	OS   string `json:"os"`
	Port int    `json:"port"`
}

type pairResp struct {
	Accepted bool   `json:"accepted"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	OS       string `json:"os"`
	Reason   string `json:"reason,omitempty"`
}

// pairRequest is an incoming pairing request waiting for the local user.
type pairRequest struct {
	ReqID    string
	ID       string
	Name     string
	OS       string
	FP       string
	Code     string
	Addr     string
	Created  time.Time
	decision chan bool
}

func (a *App) startPeerServer() error {
	ln, err := net.Listen("tcp", net.JoinHostPort(os.Getenv("SHUTTLE_BIND"), strconv.Itoa(a.cfg.PeerPort)))
	if err != nil {
		return fmt.Errorf("cannot listen on port %d (is another copy of Shuttle running?): %w", a.cfg.PeerPort, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/hello", a.hHello)
	mux.HandleFunc("POST /v1/pair", a.hPair)
	mux.HandleFunc("POST /v1/unpair", a.auth(a.hUnpair))
	mux.HandleFunc("POST /v1/offer", a.auth(a.hOffer))
	mux.HandleFunc("GET /v1/status/{tid}/{idx}", a.auth(a.hStatus))
	mux.HandleFunc("PUT /v1/upload/{tid}/{idx}", a.auth(a.hUpload))
	mux.HandleFunc("POST /v1/commit/{tid}/{idx}", a.auth(a.hCommit))
	mux.HandleFunc("POST /v1/discard/{tid}/{idx}", a.auth(a.hDiscard))
	mux.HandleFunc("POST /v1/finish/{tid}", a.auth(a.hFinish))

	srv := &http.Server{
		Handler: mux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{a.cert},
			ClientAuth:   tls.RequireAnyClientCert, // checked against paired list in auth()
			MinVersion:   tls.VersionTLS13,
			NextProtos:   []string{"http/1.1"},
		},
		TLSNextProto:      map[string]func(*http.Server, *tls.Conn, http.Handler){}, // HTTP/1.1 only
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       90 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	go func() {
		if err := srv.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed {
			log.Printf("peer server stopped: %v", err)
		}
	}()
	return nil
}

func clientFP(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	return fingerprint(r.TLS.PeerCertificates[0].Raw)
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// auth only lets paired devices through.
func (a *App) auth(h func(http.ResponseWriter, *http.Request, Peer)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.peerByFP(clientFP(r))
		if !ok {
			httpErr(w, http.StatusForbidden, "this device is not paired")
			return
		}
		a.mu.Lock()
		a.seen[p.ID] = time.Now()
		a.mu.Unlock()
		h(w, r, p)
	}
}

func (a *App) hHello(w http.ResponseWriter, r *http.Request) {
	_, paired := a.peerByFP(clientFP(r))
	a.mu.Lock()
	resp := helloResp{ID: a.cfg.DeviceID, Name: a.cfg.DeviceName, OS: runtime.GOOS, Version: a.version, Paired: paired}
	a.mu.Unlock()
	writeJSON(w, 200, resp)
}

func (a *App) hPair(w http.ResponseWriter, r *http.Request) {
	var req pairReq
	if err := readJSON(r, &req); err != nil || req.ID == "" || req.Port <= 0 || req.Port > 65535 {
		httpErr(w, 400, "bad pair request")
		return
	}
	fp := clientFP(r)
	if fp == "" {
		httpErr(w, 400, "no client certificate")
		return
	}
	addr := net.JoinHostPort(remoteIP(r), strconv.Itoa(req.Port))
	a.mu.Lock()
	me := pairResp{Accepted: true, ID: a.cfg.DeviceID, Name: a.cfg.DeviceName, OS: runtime.GOOS}
	a.mu.Unlock()

	// Already paired with this exact identity: just confirm.
	if p, ok := a.peerByFP(fp); ok && p.ID == req.ID {
		p.Name, p.OS, p.Addr = req.Name, req.OS, addr
		a.addPeer(p)
		writeJSON(w, 200, me)
		return
	}

	pr := &pairRequest{
		ReqID: randomHex(8), ID: req.ID, Name: req.Name, OS: req.OS, FP: fp,
		Code: pairCode(a.fp, fp), Addr: addr, Created: time.Now(), decision: make(chan bool, 1),
	}
	a.mu.Lock()
	a.pairIn[pr.ReqID] = pr
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.pairIn, pr.ReqID)
		a.mu.Unlock()
	}()
	log.Printf("pairing request from %q (%s), code %s", req.Name, addr, pr.Code)
	a.attention("Pairing request", fmt.Sprintf("%s wants to pair. Code: %s", req.Name, pr.Code))

	var accepted bool
	select {
	case accepted = <-pr.decision:
	case <-time.After(2 * time.Minute):
		writeJSON(w, 200, pairResp{Accepted: false, Reason: "timed out waiting for approval"})
		return
	case <-r.Context().Done():
		return
	}
	if !accepted {
		writeJSON(w, 200, pairResp{Accepted: false, Reason: "declined"})
		return
	}
	a.addPeer(Peer{ID: req.ID, Name: req.Name, OS: req.OS, Fingerprint: fp, Addr: addr, PairedAt: time.Now()})
	log.Printf("paired with %q", req.Name)
	writeJSON(w, 200, me)
}

func (a *App) hUnpair(w http.ResponseWriter, r *http.Request, p Peer) {
	a.removePeer(p.ID)
	log.Printf("%q unpaired from us", p.Name)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// attention notifies the user when the UI isn't open.
func (a *App) attention(title, msg string) {
	if a.uiClients.Load() == 0 {
		go platform.Notify(title, msg)
		if title == "Pairing request" {
			go a.launchWindow()
		}
	}
}
