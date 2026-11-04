// Package platform contains the small amount of OS-specific glue Shuttle needs:
// native file dialogs, opening URLs/folders, free disk space, keeping the machine
// awake, start-at-login and notifications.
package platform

import (
	"errors"
	"os"
	"strings"
)

var ErrUnsupported = errors.New("not supported on this OS")

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "My computer"
	}
	return strings.TrimSuffix(h, ".local")
}
