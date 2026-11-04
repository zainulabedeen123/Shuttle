package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(v)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

var (
	reservedWinName = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[0-9]|lpt[0-9])(\..*)?$`)
	tidPattern      = regexp.MustCompile(`^[a-f0-9]{16,64}$`)
)

// sanitizeRelPath turns a sender-supplied relative path into a safe one that is
// valid on both Windows and macOS. It rejects anything that could escape the
// download directory.
func sanitizeRelPath(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	if p == "" || strings.HasPrefix(p, "/") || len(p) > 4096 {
		return "", fmt.Errorf("invalid path %q", p)
	}
	parts := strings.Split(p, "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("invalid path %q", p)
		}
		var b strings.Builder
		for _, r := range part {
			switch {
			case r < 32, strings.ContainsRune(`<>:"|?*`, r):
				b.WriteRune('_')
			default:
				b.WriteRune(r)
			}
		}
		s := strings.TrimRight(b.String(), " .")
		if s == "" {
			s = "_"
		}
		if reservedWinName.MatchString(s) {
			s = "_" + s
		}
		parts[i] = s
	}
	return strings.Join(parts, "/"), nil
}

// uniquePath returns p, or "name (n).ext" if p already exists.
func uniquePath(p string) string {
	if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
		return p
	}
	dir, base := filepath.Split(p)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; ; i++ {
		c := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		if _, err := os.Lstat(c); errors.Is(err, os.ErrNotExist) {
			return c
		}
	}
}

func isJunk(name string) bool {
	switch name {
	case ".DS_Store", "Thumbs.db", "desktop.ini", ".localized", partsDirName:
		return true
	}
	return strings.HasPrefix(name, "._")
}

// displayName summarises a list of relative paths for the UI.
func displayName(rels []string) string {
	if len(rels) == 0 {
		return ""
	}
	if len(rels) == 1 {
		return rels[0]
	}
	var tops []string
	seen := map[string]bool{}
	for _, r := range rels {
		t := strings.SplitN(r, "/", 2)[0]
		if !seen[t] {
			seen[t] = true
			tops = append(tops, t)
		}
	}
	switch len(tops) {
	case 1:
		return tops[0]
	case 2:
		return tops[0] + " + " + tops[1]
	}
	return fmt.Sprintf("%s + %d more", tops[0], len(tops)-1)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func sleepCtx(ctx interface{ Done() <-chan struct{} }, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func backoff(attempt int) time.Duration {
	if attempt > 5 {
		return 20 * time.Second
	}
	return time.Duration(1<<attempt) * 500 * time.Millisecond
}

// usableIPv4Nets returns IPv4 networks of interfaces that are up and not loopback.
func usableIPv4Nets() []*net.IPNet {
	var out []*net.IPNet
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, ad := range addrs {
			ipn, ok := ad.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || ip4.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, &net.IPNet{IP: ip4, Mask: ipn.Mask})
		}
	}
	return out
}

func localIPs() []string {
	var out []string
	for _, n := range usableIPv4Nets() {
		out = append(out, n.IP.String())
	}
	return out
}

func broadcastAddrs() []net.IP {
	var out []net.IP
	for _, n := range usableIPv4Nets() {
		mask := n.Mask
		if len(mask) == 16 {
			mask = mask[12:]
		}
		if len(mask) != 4 {
			continue
		}
		b := make(net.IP, 4)
		for i := 0; i < 4; i++ {
			b[i] = n.IP[i] | ^mask[i]
		}
		out = append(out, b)
	}
	return out
}

// normalizeAddr accepts "192.168.1.5", "192.168.1.5:47820", "http://host" etc.
func normalizeAddr(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimRight(s, "/")
	if s == "" {
		return "", errors.New("enter an IP address")
	}
	if host, port, err := net.SplitHostPort(s); err == nil {
		if host == "" {
			return "", errors.New("invalid address")
		}
		return net.JoinHostPort(host, port), nil
	}
	return net.JoinHostPort(strings.Trim(s, "[]"), fmt.Sprint(defaultPeerPort)), nil
}
