package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSanitizeRelPath(t *testing.T) {
	ok := map[string]string{
		"video.mov":             "video.mov",
		"Photos/2024/img.jpg":   "Photos/2024/img.jpg",
		`Photos\win\img.jpg`:    "Photos/win/img.jpg",
		"a:b?.txt":              "a_b_.txt",
		"CON.txt":               "_CON.txt",
		"dir/trailing. ":        "dir/trailing",
		"日本語/ファイル.txt":          "日本語/ファイル.txt",
		"folder/with space.mp4": "folder/with space.mp4",
	}
	for in, want := range ok {
		got, err := sanitizeRelPath(in)
		if err != nil || got != want {
			t.Errorf("sanitizeRelPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "/etc/passwd", "../secret", "a/../../b", "a//b", `\\server\share`, "./x", "a/."} {
		if got, err := sanitizeRelPath(bad); err == nil {
			t.Errorf("sanitizeRelPath(%q) = %q; want error", bad, got)
		}
	}
}

func TestUniquePath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "movie.mov")
	if got := uniquePath(p); got != p {
		t.Fatalf("got %s", got)
	}
	os.WriteFile(p, nil, 0o644)
	if got := uniquePath(p); got != filepath.Join(dir, "movie (1).mov") {
		t.Fatalf("got %s", got)
	}
}

func TestPairCodeSymmetric(t *testing.T) {
	a, b := fingerprint([]byte("A")), fingerprint([]byte("B"))
	if pairCode(a, b) != pairCode(b, a) {
		t.Fatal("pair code must be identical on both devices")
	}
	if pairCode(a, b) == pairCode(a, fingerprint([]byte("C"))) {
		t.Fatal("different identities should give different codes")
	}
}

func TestDisplayName(t *testing.T) {
	cases := map[string][]string{
		"a.mov":          {"a.mov"},
		"Photos":         {"Photos/1.jpg", "Photos/2.jpg"},
		"a.mov + Photos": {"a.mov", "Photos/1.jpg", "Photos/2.jpg"},
		"a.mov + 2 more": {"a.mov", "b.mov", "c.mov"},
	}
	for want, in := range cases {
		if got := displayName(in); got != want {
			t.Errorf("displayName(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeAddr(t *testing.T) {
	cases := map[string]string{
		"192.168.1.5":        "192.168.1.5:47820",
		" 192.168.1.5:9000 ": "192.168.1.5:9000",
		"http://10.0.0.2/":   "10.0.0.2:47820",
	}
	for in, want := range cases {
		if got, err := normalizeAddr(in); err != nil || got != want {
			t.Errorf("normalizeAddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
