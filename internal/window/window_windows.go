//go:build windows

// Package window shows Shuttle's UI in a native app window (WebView2 on
// Windows, WebKit on macOS) instead of a browser tab.
package window

import (
	"errors"

	webview2 "github.com/jchv/go-webview2"
)

// Open shows url in a native window and blocks until the window is closed.
// Must be called on the main OS thread.
func Open(url, dataDir string) error {
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		DataPath:  dataDir,
		WindowOptions: webview2.WindowOptions{
			Title: "Shuttle", Width: 1180, Height: 820, IconId: 1, Center: true,
		},
	})
	if w == nil {
		return errors.New("Microsoft Edge WebView2 runtime is not available")
	}
	defer w.Destroy()
	_ = w.Bind("shuttleCloseWindow", func() { w.Terminate() })
	w.Init("window.__SHUTTLE_APP = true;")
	w.Navigate(url)
	w.Run()
	return nil
}
