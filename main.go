// Shuttle — fast, verified, resumable file transfer between Windows and macOS on your LAN.
//
// Shuttle has two parts in one executable:
//   - the background service (`shuttle run`), which discovers devices and
//     sends/receives files; it starts at login and keeps running;
//   - the app window (`shuttle` with no arguments), a native window showing the
//     service's UI. Closing the window doesn't stop transfers.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"

	"shuttle/internal/app"
	"shuttle/internal/platform"
)

var version = "1.1.0"

// Native windows (WebView2 / Cocoa) must run on the process's main thread.
func init() { runtime.LockOSThread() }

func usage() {
	fmt.Fprintf(os.Stderr, `Shuttle %s — send files between your Windows PC and Mac

Usage:
  shuttle                         open the Shuttle window (starts the service if needed)
  shuttle run                     run the background service in the foreground
  shuttle send [--to NAME] PATH…  send files/folders to a paired device
  shuttle version

Advanced (for testing two copies on one machine):
  SHUTTLE_HOME=dir shuttle run --peer-port N --ui-port N --discovery-port N
`, version)
}

func main() {
	platform.AttachConsole()

	var args []string
	for _, a := range os.Args[1:] {
		if !strings.HasPrefix(a, "-psn_") { // macOS Finder process serial number
			args = append(args, a)
		}
	}
	cmd := "app"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "app", "open":
		err = app.OpenWindow()
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		_ = fs.Bool("no-browser", true, "(kept for compatibility; the service never opens a window by itself)")
		open := fs.Bool("open", false, "also open the app window")
		peerPort := fs.Int("peer-port", 0, "TCP port for transfers (default 47820)")
		uiPort := fs.Int("ui-port", 0, "local UI port (default 47821)")
		discPort := fs.Int("discovery-port", 0, "UDP discovery port (default 47822)")
		_ = fs.Parse(args)
		err = app.Run(app.Options{OpenWindow: *open, PeerPort: *peerPort, UIPort: *uiPort, DiscoveryPort: *discPort, Version: version})
	case "send":
		fs := flag.NewFlagSet("send", flag.ExitOnError)
		to := fs.String("to", "", "name of the paired device (default: the online one)")
		_ = fs.Parse(args)
		if fs.NArg() == 0 {
			usage()
			os.Exit(2)
		}
		err = app.SendFromCLI(*to, fs.Args())
	case "version":
		fmt.Println("Shuttle", version)
	case "help":
		usage()
	default:
		if _, statErr := os.Stat(cmd); statErr == nil { // files dropped onto the exe
			err = app.SendFromCLI("", append([]string{cmd}, args...))
		} else {
			usage()
			os.Exit(2)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "shuttle:", err)
		platform.Notify("Shuttle", err.Error())
		os.Exit(1)
	}
}
