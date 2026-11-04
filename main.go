// Shuttle — fast, verified, resumable file transfer between Windows and macOS on your LAN.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"shuttle/internal/app"
	"shuttle/internal/platform"
)

var version = "1.0.0"

func usage() {
	fmt.Fprintf(os.Stderr, `Shuttle %s — send files between your Windows PC and Mac

Usage:
  shuttle                         start Shuttle and open its window
  shuttle run [--no-browser]      run in the foreground
  shuttle send [--to NAME] PATH…  send files/folders to a paired device
  shuttle open                    open the window of the running instance
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

	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	if cmd == "" {
		// Inside a macOS .app bundle the launcher must exit so the app can be
		// re-opened from Finder; Shuttle itself keeps running in the background.
		if runtime.GOOS == "darwin" && strings.Contains(exePath(), ".app/Contents/MacOS/") && len(args) == 0 {
			cmd = "open"
		} else {
			cmd = "run"
		}
	}

	var err error
	switch cmd {
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		noBrowser := fs.Bool("no-browser", false, "don't open the browser window")
		peerPort := fs.Int("peer-port", 0, "TCP port for transfers (default 47820)")
		uiPort := fs.Int("ui-port", 0, "local UI port (default 47821)")
		discPort := fs.Int("discovery-port", 0, "UDP discovery port (default 47822)")
		_ = fs.Parse(args)
		err = app.Run(app.Options{NoBrowser: *noBrowser, PeerPort: *peerPort, UIPort: *uiPort, DiscoveryPort: *discPort, Version: version})
	case "send":
		fs := flag.NewFlagSet("send", flag.ExitOnError)
		to := fs.String("to", "", "name of the paired device (default: the online one)")
		_ = fs.Parse(args)
		if fs.NArg() == 0 {
			usage()
			os.Exit(2)
		}
		err = app.SendFromCLI(*to, fs.Args())
	case "open":
		err = app.OpenUI()
	case "version":
		fmt.Println("Shuttle", version)
	case "help":
		usage()
	default:
		if _, statErr := os.Stat(cmd); statErr == nil { // `shuttle file1 file2` (drag onto exe)
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

func exePath() string {
	p, _ := os.Executable()
	return filepath.ToSlash(p)
}
