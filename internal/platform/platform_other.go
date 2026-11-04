//go:build !windows && !darwin

package platform

// Minimal Linux/BSD support (not an officially targeted platform).

import (
	"os/exec"
	"strings"
	"syscall"
)

func OpenURL(u string) error    { return exec.Command("xdg-open", u).Start() }
func OpenFolder(p string) error { return exec.Command("xdg-open", p).Start() }
func Reveal(p string) error     { return exec.Command("xdg-open", p[:strings.LastIndex(p, "/")+1]).Start() }
func HideFile(string)           {}
func ComputerName() string      { return hostname() }

func zenity(args ...string) (string, error) {
	out, err := exec.Command("zenity", args...).Output()
	if _, ok := err.(*exec.ExitError); ok {
		return "", nil // cancelled
	}
	return string(out), err
}

func PickFiles() ([]string, error) {
	out, err := zenity("--file-selection", "--multiple", "--separator=\n", "--title=Choose files to send")
	return splitLines(out), err
}

func PickFolders() ([]string, error) {
	out, err := zenity("--file-selection", "--directory", "--multiple", "--separator=\n", "--title=Choose folders to send")
	return splitLines(out), err
}

func PickFolder(title string) (string, error) {
	out, err := zenity("--file-selection", "--directory", "--title="+title)
	return strings.TrimSpace(out), err
}

func FreeSpace(path string) int64     { return statfsFree(path) }
func SetAwake(bool)                   {}
func SetAutostart(bool, string) error { return ErrUnsupported }
func AutostartEnabled() bool          { return false }
func InstallSendTo(string) error      { return ErrUnsupported }
func Notify(title, msg string)        { _ = exec.Command("notify-send", title, msg).Run() }
func AttachConsole()                  {}
func Detach(cmd *exec.Cmd)            { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
