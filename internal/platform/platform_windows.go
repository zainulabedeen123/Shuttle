//go:build windows

package platform

import (
	"encoding/base64"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

const createNoWindow = 0x08000000

var kernel32 = syscall.NewLazyDLL("kernel32.dll")

func hidden(cmd *exec.Cmd) *exec.Cmd {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return cmd
}

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func powershell(script string) (string, error) {
	full := "$ErrorActionPreference='Stop'\n[Console]::OutputEncoding=New-Object System.Text.UTF8Encoding $false\n" + script
	u := utf16.Encode([]rune(full))
	b := make([]byte, len(u)*2)
	for i, c := range u {
		b[2*i], b[2*i+1] = byte(c), byte(c>>8)
	}
	enc := base64.StdEncoding.EncodeToString(b)
	out, err := hidden(exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-STA",
		"-ExecutionPolicy", "Bypass", "-EncodedCommand", enc)).Output()
	return strings.TrimPrefix(strings.TrimSpace(string(out)), string(rune(0xFEFF))), err
}

func OpenURL(u string) error {
	return hidden(exec.Command("rundll32", "url.dll,FileProtocolHandler", u)).Start()
}

func OpenFolder(p string) error { return exec.Command("explorer", p).Start() }

func Reveal(p string) error {
	cmd := exec.Command("explorer")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer /select,"` + p + `"`}
	return cmd.Start()
}

const dialogOwner = `Add-Type -AssemblyName System.Windows.Forms
[System.Windows.Forms.Application]::EnableVisualStyles()
$o = New-Object System.Windows.Forms.Form -Property @{TopMost=$true}
`

func PickFiles() ([]string, error) {
	out, err := powershell(dialogOwner + `$d = New-Object System.Windows.Forms.OpenFileDialog
$d.Multiselect = $true
$d.Title = 'Choose files to send'
if ($d.ShowDialog($o) -eq [System.Windows.Forms.DialogResult]::OK) { $d.FileNames -join "` + "`n" + `" }`)
	return splitLines(out), err
}

func PickFolders() ([]string, error) {
	p, err := PickFolder("Choose a folder to send")
	if p == "" {
		return nil, err
	}
	return []string{p}, err
}

func PickFolder(title string) (string, error) {
	out, err := powershell(dialogOwner + `$d = New-Object System.Windows.Forms.FolderBrowserDialog
$d.Description = ` + psQuote(title) + `
$d.ShowNewFolderButton = $true
if ($d.ShowDialog($o) -eq [System.Windows.Forms.DialogResult]::OK) { $d.SelectedPath }`)
	return strings.TrimSpace(out), err
}

func FreeSpace(path string) int64 {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return -1
	}
	var free, total, totalFree uint64
	r, _, _ := kernel32.NewProc("GetDiskFreeSpaceExW").Call(uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&free)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&totalFree)))
	if r == 0 {
		return -1
	}
	return int64(free)
}

var (
	awakeOnce sync.Once
	awakeCh   chan bool
)

// SetAwake prevents (or allows again) system sleep. SetThreadExecutionState is
// per-thread, so all calls happen on one locked OS thread.
func SetAwake(on bool) {
	awakeOnce.Do(func() {
		awakeCh = make(chan bool, 16)
		go func() {
			runtime.LockOSThread()
			proc := kernel32.NewProc("SetThreadExecutionState")
			for v := range awakeCh {
				flags := uintptr(0x80000000) // ES_CONTINUOUS
				if v {
					flags |= 0x00000001 // ES_SYSTEM_REQUIRED
				}
				_, _, _ = proc.Call(flags)
			}
		}()
	})
	select {
	case awakeCh <- on:
	default:
	}
}

const runKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

func SetAutostart(on bool, exe string) error {
	if on {
		return hidden(exec.Command("reg", "add", runKey, "/v", "Shuttle", "/t", "REG_SZ",
			"/d", `"`+exe+`" run --no-browser`, "/f")).Run()
	}
	err := hidden(exec.Command("reg", "delete", runKey, "/v", "Shuttle", "/f")).Run()
	if !AutostartEnabled() {
		return nil
	}
	return err
}

func AutostartEnabled() bool {
	return hidden(exec.Command("reg", "query", runKey, "/v", "Shuttle")).Run() == nil
}

// InstallSendTo adds "Shuttle" to Explorer's right-click → Send to menu.
func InstallSendTo(exe string) error {
	_, err := powershell(`$p = Join-Path ([Environment]::GetFolderPath('SendTo')) 'Shuttle.lnk'
$s = (New-Object -ComObject WScript.Shell).CreateShortcut($p)
$s.TargetPath = ` + psQuote(exe) + `
$s.Arguments = 'send'
$s.IconLocation = ` + psQuote(exe+",0") + `
$s.Description = 'Send to your other computer with Shuttle'
$s.Save()`)
	return err
}

func Notify(title, msg string) {
	_, _ = powershell(`Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$n = New-Object System.Windows.Forms.NotifyIcon
$n.Icon = [System.Drawing.SystemIcons]::Information
$n.Visible = $true
$n.ShowBalloonTip(6000, ` + psQuote(title) + `, ` + psQuote(msg) + `, [System.Windows.Forms.ToolTipIcon]::Info)
Start-Sleep -Seconds 7
$n.Dispose()`)
}

func ComputerName() string { return hostname() }

// AttachConsole lets the GUI-subsystem build print to the terminal it was started from.
func AttachConsole() {
	r, _, _ := kernel32.NewProc("AttachConsole").Call(uintptr(^uint32(0))) // ATTACH_PARENT_PROCESS
	if r == 0 {
		return
	}
	if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		os.Stdout, os.Stderr = f, f
	}
}

func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000008 | 0x00000200} // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
}

func HideFile(path string) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	attrs, err := syscall.GetFileAttributes(p)
	if err != nil {
		return
	}
	_ = syscall.SetFileAttributes(p, attrs|syscall.FILE_ATTRIBUTE_HIDDEN)
}

// FirewallWarning reports when Windows Defender Firewall has an inbound Block
// rule for Shuttle on the current network profile (e.g. "Cancel" was clicked on
// the first-run prompt, or only Private was allowed while on a Public network).
func FirewallWarning(exe string) string {
	out, err := powershell(`$p = ` + psQuote(exe) + `
$cat = [string](Get-NetConnectionProfile | Select-Object -First 1).NetworkCategory
$rules = Get-NetFirewallApplicationFilter | Where-Object { $_.Program -eq $p } | Get-NetFirewallRule | Where-Object { $_.Enabled -eq 'True' -and $_.Direction -eq 'Inbound' }
$blocked = $rules | Where-Object { $_.Action -eq 'Block' -and ([string]$_.Profile -eq 'Any' -or ([string]$_.Profile).Contains($cat)) }
if ($blocked) { "BLOCK:" + $cat }`)
	if err != nil || !strings.HasPrefix(out, "BLOCK:") {
		return ""
	}
	return "Windows Firewall is blocking Shuttle on this " + strings.TrimPrefix(out, "BLOCK:") +
		" network, so your other computer can't connect to this PC. Fix: Windows Security → Firewall & network protection → Allow an app through firewall → tick both Private and Public for Shuttle."
}
