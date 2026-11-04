//go:build darwin

package platform

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

func OpenURL(u string) error    { return exec.Command("open", u).Start() }
func OpenFolder(p string) error { return exec.Command("open", p).Start() }
func Reveal(p string) error     { return exec.Command("open", "-R", p).Start() }
func asQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
func HideFile(string) {} // dot-prefixed names are already hidden
func ComputerName() string {
	if out, err := exec.Command("scutil", "--get", "ComputerName").Output(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return s
		}
	}
	return hostname()
}

func osascript(script string) (string, error) {
	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && strings.Contains(string(ee.Stderr), "-128") { // user cancelled
			return "", nil
		}
		return "", err
	}
	return string(out), nil
}

func PickFiles() ([]string, error) {
	out, err := osascript(`activate
set fs to choose file with prompt "Choose files to send" with multiple selections allowed
set out to ""
repeat with i from 1 to count of fs
	set out to out & POSIX path of (item i of fs) & linefeed
end repeat
return out`)
	return splitLines(out), err
}

func PickFolders() ([]string, error) {
	out, err := osascript(`activate
set fs to choose folder with prompt "Choose folders to send" with multiple selections allowed
set out to ""
repeat with i from 1 to count of fs
	set out to out & POSIX path of (item i of fs) & linefeed
end repeat
return out`)
	var res []string
	for _, p := range splitLines(out) {
		res = append(res, filepath.Clean(p))
	}
	return res, err
}

func PickFolder(title string) (string, error) {
	out, err := osascript("activate\nreturn POSIX path of (choose folder with prompt " + asQuote(title) + ")")
	out = strings.TrimSpace(out)
	if out == "" {
		return "", err
	}
	return filepath.Clean(out), err
}

func FreeSpace(path string) int64 { return statfsFree(path) }

var (
	awakeMu sync.Mutex
	caff    *exec.Cmd
)

// SetAwake runs `caffeinate -i` (prevent idle sleep) tied to our PID.
func SetAwake(on bool) {
	awakeMu.Lock()
	defer awakeMu.Unlock()
	if on && caff == nil {
		c := exec.Command("caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
		if c.Start() == nil {
			caff = c
			go func() { _ = c.Wait() }()
		}
	} else if !on && caff != nil {
		_ = caff.Process.Kill()
		caff = nil
	}
}

func plistPath() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, "Library", "LaunchAgents", "app.shuttle.transfer.plist")
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

func SetAutostart(on bool, exe string) error {
	p := plistPath()
	if !on {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>app.shuttle.transfer</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEscape(exe) + `</string>
		<string>run</string>
		<string>--no-browser</string>
	</array>
	<key>RunAtLoad</key><true/>
	<key>ProcessType</key><string>Interactive</string>
</dict>
</plist>
`
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(plist), 0o644)
}

func AutostartEnabled() bool {
	_, err := os.Stat(plistPath())
	return err == nil
}

func InstallSendTo(string) error { return ErrUnsupported }

func Notify(title, msg string) {
	_, _ = osascript("display notification " + asQuote(msg) + " with title " + asQuote(title))
}

func AttachConsole() {}

func Detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// FirewallWarning reports when the macOS Application Firewall blocks Shuttle.
func FirewallWarning(exe string) string {
	const fw = "/usr/libexec/ApplicationFirewall/socketfilterfw"
	out, err := exec.Command(fw, "--getglobalstate").Output()
	if err != nil || !strings.Contains(strings.ToLower(string(out)), "enabled") {
		return ""
	}
	const fix = " Open System Settings → Network → Firewall → Options…, turn off “Block all incoming connections” and set Shuttle to “Allow incoming connections”."
	if o, _ := exec.Command(fw, "--getblockall").Output(); strings.Contains(strings.ToLower(string(o)), "enabled") {
		return "macOS Firewall is blocking all incoming connections, so your other computer can't connect to this Mac." + fix
	}
	target := exe
	if i := strings.Index(exe, ".app/Contents/MacOS/"); i > 0 {
		target = exe[:i+4]
	}
	o, _ := exec.Command(fw, "--getappblocked", target).Output()
	s := strings.ToLower(string(o))
	if strings.Contains(s, "block") && !strings.Contains(s, "permitted") {
		return "macOS Firewall is blocking Shuttle, so your other computer can't connect to this Mac." + fix
	}
	return ""
}
