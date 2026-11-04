// Command build cross-compiles Shuttle for Windows and macOS from any OS:
//
//	go run ./tools/build
//
// Outputs (in dist/):
//
//	Shuttle-Windows.exe   – Windows 10/11 x64 (no console window)
//	Shuttle-macOS.zip     – Shuttle.app, universal (Apple Silicon + Intel)
package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const version = "1.0.0"

func main() {
	must(os.MkdirAll("dist", 0o755))
	ld := "-s -w -X main.version=" + version

	build("windows", "amd64", "dist/Shuttle-Windows.exe", ld+" -H windowsgui")
	build("darwin", "arm64", "dist/.shuttle-darwin-arm64", ld)
	build("darwin", "amd64", "dist/.shuttle-darwin-amd64", ld)

	fat := lipo("dist/.shuttle-darwin-arm64", "dist/.shuttle-darwin-amd64")
	must(os.WriteFile("dist/shuttle-macos-universal", fat, 0o755))
	writeAppZip("dist/Shuttle-macOS.zip", fat)
	_ = os.Remove("dist/.shuttle-darwin-arm64")
	_ = os.Remove("dist/.shuttle-darwin-amd64")
	fmt.Println("done → dist/")
}

func build(goos, arch, out, ldflags string) {
	fmt.Printf("building %s/%s → %s\n", goos, arch, out)
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", out, ".")
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+arch, "CGO_ENABLED=0")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	must(cmd.Run())
}

// lipo creates a Mach-O universal ("fat") binary. Each slice keeps the ad-hoc
// code signature the Go linker adds, which Apple Silicon requires.
func lipo(paths ...string) []byte {
	type slice struct {
		data          []byte
		cpu, sub      uint32
		align, offset uint32
	}
	var slices []slice
	for _, p := range paths {
		d, err := os.ReadFile(p)
		must(err)
		if binary.LittleEndian.Uint32(d[0:4]) != 0xfeedfacf {
			must(fmt.Errorf("%s is not a 64-bit Mach-O file", p))
		}
		slices = append(slices, slice{data: d, cpu: binary.LittleEndian.Uint32(d[4:8]), sub: binary.LittleEndian.Uint32(d[8:12]), align: 14})
	}
	off := uint32(8 + 20*len(slices))
	for i := range slices {
		a := uint32(1) << slices[i].align
		off = (off + a - 1) &^ (a - 1)
		slices[i].offset = off
		off += uint32(len(slices[i].data))
	}
	var buf bytes.Buffer
	be := func(v uint32) { _ = binary.Write(&buf, binary.BigEndian, v) }
	be(0xcafebabe)
	be(uint32(len(slices)))
	for _, s := range slices {
		be(s.cpu)
		be(s.sub)
		be(s.offset)
		be(uint32(len(s.data)))
		be(s.align)
	}
	for _, s := range slices {
		buf.Write(make([]byte, int(s.offset)-buf.Len()))
		buf.Write(s.data)
	}
	return buf.Bytes()
}

const infoPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key><string>Shuttle</string>
	<key>CFBundleDisplayName</key><string>Shuttle</string>
	<key>CFBundleIdentifier</key><string>app.shuttle.transfer</string>
	<key>CFBundleVersion</key><string>VERSION</string>
	<key>CFBundleShortVersionString</key><string>VERSION</string>
	<key>CFBundleExecutable</key><string>shuttle</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>LSMinimumSystemVersion</key><string>11.0</string>
	<key>LSUIElement</key><true/>
	<key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
`

const macReadme = `Shuttle for macOS
=================

1. Drag Shuttle.app into your Applications folder.
2. First launch only: macOS blocks apps that aren't from the App Store.
   Open Terminal and run:

       xattr -dr com.apple.quarantine /Applications/Shuttle.app

   (or right-click Shuttle.app → Open → Open).
3. Double-click Shuttle. Your browser opens the Shuttle window.
   If macOS asks "Accept incoming network connections?", click Allow.

Command line (optional):

    /Applications/Shuttle.app/Contents/MacOS/shuttle send ~/Movies/big.mov
`

// writeAppZip packages Shuttle.app with correct Unix permissions (a zip made by
// Windows Explorer would lose the executable bit).
func writeAppZip(out string, exe []byte) {
	f, err := os.Create(out)
	must(err)
	defer f.Close()
	zw := zip.NewWriter(f)
	add := func(name string, mode os.FileMode, data []byte) {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Now()}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		must(err)
		_, err = io.Copy(w, bytes.NewReader(data))
		must(err)
	}
	dir := func(name string) {
		h := &zip.FileHeader{Name: name + "/", Modified: time.Now()}
		h.SetMode(os.ModeDir | 0o755)
		_, err := zw.CreateHeader(h)
		must(err)
	}
	dir("Shuttle.app")
	dir("Shuttle.app/Contents")
	dir("Shuttle.app/Contents/MacOS")
	add("Shuttle.app/Contents/Info.plist", 0o644, []byte(strings.ReplaceAll(infoPlist, "VERSION", version)))
	add("Shuttle.app/Contents/PkgInfo", 0o644, []byte("APPL????"))
	add("Shuttle.app/Contents/MacOS/shuttle", 0o755, exe)
	add("READ ME FIRST.txt", 0o644, []byte(macReadme))
	must(zw.Close())
	fmt.Println("packaged", out)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "build:", err)
		os.Exit(1)
	}
}
