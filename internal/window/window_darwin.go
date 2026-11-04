//go:build darwin && cgo

package window

import (
	"errors"

	webview "github.com/webview/webview_go"
)

// Open shows url in a native WebKit window and blocks until it is closed.
// Must be called on the main OS thread.
func Open(url, dataDir string) error {
	w := webview.New(false)
	if w == nil {
		return errors.New("could not create window")
	}
	defer w.Destroy()
	w.SetTitle("Shuttle")
	w.SetSize(1180, 820, webview.HintNone)
	_ = w.Bind("shuttleCloseWindow", func() { w.Dispatch(w.Terminate) })
	w.Init("window.__SHUTTLE_APP = true;")
	w.Navigate(url)
	w.Run()
	return nil
}
