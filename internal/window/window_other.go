//go:build !windows && !(darwin && cgo)

package window

import "errors"

// ErrUnsupported means no native window is available in this build; callers
// fall back to the default browser. (macOS builds need cgo for WebKit.)
var ErrUnsupported = errors.New("native window not available in this build")

func Open(url, dataDir string) error { return ErrUnsupported }
