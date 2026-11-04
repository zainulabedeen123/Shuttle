package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// httpError is a non-2xx response from a peer.
type httpError struct {
	Code   int
	Msg    string
	Offset *int64
}

func (e *httpError) Error() string { return e.Msg }

// permanentError marks errors that retrying will not fix.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

func permanent(err error) error { return permanentError{err} }

var errMismatch = errors.New("checksum mismatch")

func retryable(err error) bool {
	if err == nil {
		return false
	}
	var pe permanentError
	if errors.As(err, &pe) {
		return false
	}
	var he *httpError
	if errors.As(err, &he) {
		switch he.Code {
		case 404, 408, 409, 425, 429:
			return true
		case 507:
			return false
		}
		return he.Code >= 500
	}
	return true // network errors
}

// deadlineConn sets a write deadline before every write so that a vanished peer
// (Wi-Fi drop, laptop lid closed) is detected quickly instead of hanging for minutes.
type deadlineConn struct {
	net.Conn
	d time.Duration
}

func (c *deadlineConn) Write(b []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(c.d))
	return c.Conn.Write(b)
}

// capturedFP receives the fingerprint of the server certificate seen in the handshake.
type capturedFP struct {
	mu sync.Mutex
	fp string
}

func (c *capturedFP) get() string { c.mu.Lock(); defer c.mu.Unlock(); return c.fp }

// newPeerClient returns an HTTPS client that presents our certificate and, if pin
// is non-empty, only talks to a server whose certificate fingerprint equals pin.
func (a *App) newPeerClient(pin string, capture *capturedFP, respTimeout time.Duration) *http.Client {
	tlsCfg := &tls.Config{
		Certificates:       []tls.Certificate{a.cert},
		InsecureSkipVerify: true, // self-signed; verified by pinning below
		MinVersion:         tls.VersionTLS13,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("peer presented no certificate")
			}
			fp := fingerprint(cs.PeerCertificates[0].Raw)
			if pin != "" && fp != pin {
				return errors.New("device identity mismatch — it may have been reinstalled; unpair and pair again")
			}
			if capture != nil {
				capture.mu.Lock()
				capture.fp = fp
				capture.mu.Unlock()
			}
			return nil
		},
	}
	dialer := &net.Dialer{Timeout: 6 * time.Second, KeepAlive: 15 * time.Second}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetNoDelay(true)
				_ = tc.SetWriteBuffer(4 << 20)
			}
			return &deadlineConn{Conn: c, d: 45 * time.Second}, nil
		},
		TLSClientConfig:       tlsCfg,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: respTimeout,
		WriteBufferSize:       256 << 10,
		ReadBufferSize:        64 << 10,
		DisableCompression:    true,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{Transport: tr}
}

func peerDo(ctx context.Context, c *http.Client, method, url string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return friendlyNetErr(err)
	}
	defer resp.Body.Close()
	return decodeResp(resp, out)
}

func decodeResp(resp *http.Response, out any) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error  string `json:"error"`
			Offset *int64 `json:"offset"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(data))
		}
		if e.Error == "" {
			e.Error = resp.Status
		}
		return &httpError{Code: resp.StatusCode, Msg: e.Error, Offset: e.Offset}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func friendlyNetErr(err error) error {
	s := err.Error()
	switch {
	case strings.Contains(s, "identity mismatch"):
		return permanent(errors.New("device identity mismatch — unpair and pair again"))
	case strings.Contains(s, "connection refused"), strings.Contains(s, "actively refused"):
		return fmt.Errorf("device not reachable (is Shuttle running on it?)")
	case strings.Contains(s, "i/o timeout"), strings.Contains(s, "deadline exceeded"):
		return fmt.Errorf("connection timed out")
	}
	return err
}

// peerURL builds a URL to a paired peer using its latest known address.
func (a *App) peerURL(peerID, path string) string {
	p, _ := a.peerByID(peerID)
	return "https://" + p.Addr + path
}
