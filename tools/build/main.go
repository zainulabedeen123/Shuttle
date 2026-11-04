// Command build produces Shuttle's release packages.
//
//	go run ./tools/build windows   → dist/Shuttle-Windows.exe   (any OS)
//	go run ./tools/build mac       → dist/Shuttle-macOS.zip     (must run on macOS:
//	                                  the native window needs cgo + WebKit)
//	go run ./tools/build           → windows, plus mac when run on macOS
//
// Release builds for both platforms are produced by GitHub Actions
// (.github/workflows/release.yml) whenever a v* tag is pushed.
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/tc-hib/winres"
	"github.com/tc-hib/winres/version"
	xdraw "golang.org/x/image/draw"
)

const appVersion = "1.1.0"

func main() {
	target := "default"
	if len(os.Args) > 1 {
		target = os.Args[1]
	}
	must(os.MkdirAll("dist", 0o755))
	switch target {
	case "windows":
		buildWindows()
	case "mac":
		buildMac()
	case "icon":
		savePNG("dist/icon-1024.png", renderIcon(1024))
	default:
		buildWindows()
		if runtime.GOOS == "darwin" {
			buildMac()
		} else {
			fmt.Println("note: the macOS app (native window) must be built on a Mac — push a v* tag and GitHub Actions builds it.")
		}
	}
}

func ldflags() string { return "-s -w -X main.version=" + appVersion }

func goBuild(env []string, out, extraLD string) {
	fmt.Println("go build →", out)
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags()+extraLD, "-o", out, ".")
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	must(cmd.Run())
}

// ---------------- Windows ----------------

func buildWindows() {
	// Embed icon, version info and a per-monitor-DPI manifest (crisp window on
	// high-DPI screens) as a .syso the Go linker picks up automatically.
	rs := winres.ResourceSet{}
	icon, err := winres.NewIconFromResizedImage(renderIcon(256), []int{16, 20, 24, 32, 40, 48, 64, 128, 256})
	must(err)
	must(rs.SetIcon(winres.ID(1), icon))
	vi := version.Info{}
	vi.SetFileVersion(appVersion)
	vi.SetProductVersion(appVersion)
	must(vi.Set(0, version.ProductName, "Shuttle"))
	must(vi.Set(0, version.FileDescription, "Shuttle — file transfer between Windows and Mac"))
	must(vi.Set(0, version.CompanyName, "Shuttle"))
	rs.SetVersionInfo(vi)
	rs.SetManifest(winres.AppManifest{DPIAwareness: winres.DPIPerMonitorV2, UseCommonControlsV6: true})
	const syso = "rsrc_windows_amd64.syso"
	f, err := os.Create(syso)
	must(err)
	must(rs.WriteObject(f, winres.ArchAMD64))
	f.Close()
	defer os.Remove(syso)

	goBuild([]string{"GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0"}, "dist/Shuttle-Windows.exe", " -H windowsgui")
}

// ---------------- macOS ----------------

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
	<key>CFBundleIconFile</key><string>Shuttle</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>LSMinimumSystemVersion</key><string>11.0</string>
	<key>LSApplicationCategoryType</key><string>public.app-category.utilities</string>
	<key>NSHighResolutionCapable</key><true/>
	<key>NSLocalNetworkUsageDescription</key><string>Shuttle finds your other computers on this network and transfers files directly to them.</string>
</dict>
</plist>
`

func run(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	must(cmd.Run())
}

func buildMac() {
	if runtime.GOOS != "darwin" {
		must(fmt.Errorf("the macOS app must be built on macOS (native WebKit window needs cgo)"))
	}
	tmp, err := os.MkdirTemp("", "shuttle-mac")
	must(err)
	defer os.RemoveAll(tmp)

	for _, arch := range []struct{ goarch, clang string }{{"arm64", "arm64"}, {"amd64", "x86_64"}} {
		flags := "-arch " + arch.clang + " -mmacosx-version-min=11.0"
		goBuild([]string{"GOOS=darwin", "GOARCH=" + arch.goarch, "CGO_ENABLED=1", "CC=clang", "CXX=clang++",
			"CGO_CFLAGS=" + flags, "CGO_CXXFLAGS=" + flags, "CGO_LDFLAGS=" + flags, "MACOSX_DEPLOYMENT_TARGET=11.0"},
			filepath.Join(tmp, "shuttle-"+arch.goarch), "")
	}

	app := "dist/Shuttle.app"
	_ = os.RemoveAll(app)
	must(os.MkdirAll(app+"/Contents/MacOS", 0o755))
	must(os.MkdirAll(app+"/Contents/Resources", 0o755))
	run("lipo", "-create", "-output", app+"/Contents/MacOS/shuttle", filepath.Join(tmp, "shuttle-arm64"), filepath.Join(tmp, "shuttle-amd64"))
	must(os.WriteFile(app+"/Contents/Info.plist", []byte(strings.ReplaceAll(infoPlist, "VERSION", appVersion)), 0o644))
	must(os.WriteFile(app+"/Contents/PkgInfo", []byte("APPL????"), 0o644))

	// App icon: render PNGs into an .iconset and convert with iconutil.
	set := filepath.Join(tmp, "Shuttle.iconset")
	must(os.MkdirAll(set, 0o755))
	big := renderIcon(1024)
	for _, s := range []int{16, 32, 128, 256, 512} {
		savePNG(filepath.Join(set, fmt.Sprintf("icon_%dx%d.png", s, s)), resize(big, s))
		savePNG(filepath.Join(set, fmt.Sprintf("icon_%dx%d@2x.png", s, s)), resize(big, s*2))
	}
	run("iconutil", "-c", "icns", set, "-o", app+"/Contents/Resources/Shuttle.icns")

	// Ad-hoc sign the whole bundle so macOS treats it as one consistent app
	// (needed for the Local Network permission prompt and the firewall).
	run("codesign", "--force", "--deep", "--sign", "-", app)
	_ = os.Remove("dist/Shuttle-macOS.zip")
	run("ditto", "-c", "-k", "--keepParent", app, "dist/Shuttle-macOS.zip")
	fmt.Println("packaged dist/Shuttle-macOS.zip")
}

// ---------------- icon ----------------

// renderIcon draws the Shuttle icon (rounded square, two opposing arrows) with
// 4× supersampling for smooth edges.
func renderIcon(size int) image.Image {
	const ss = 4
	n := size * ss
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	c1 := [3]float64{0x5b, 0x5b, 0xf7}
	c2 := [3]float64{0x8a, 0x5c, 0xff}
	fn := float64(n)
	margin, radius := fn*0.06, fn*0.22
	inRounded := func(x, y float64) bool {
		if x < margin || y < margin || x > fn-margin || y > fn-margin {
			return false
		}
		cx := math.Max(margin+radius, math.Min(x, fn-margin-radius))
		cy := math.Max(margin+radius, math.Min(y, fn-margin-radius))
		return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= radius*radius
	}
	// arrow: shaft from x0..x1 at height y (thickness t), head at the tip.
	arrow := func(x, y, yc, left, right float64, pointRight bool) bool {
		t, head := fn*0.075, fn*0.16
		tip, base := right, right-head
		if !pointRight {
			tip, base = left, left+head
		}
		inShaft := math.Abs(y-yc) <= t/2 && x >= math.Min(left, right) && x <= math.Max(left, right) &&
			((pointRight && x <= base) || (!pointRight && x >= base))
		// triangular head
		d := math.Abs(x - tip)
		inHead := ((pointRight && x >= base && x <= tip) || (!pointRight && x <= base && x >= tip)) &&
			math.Abs(y-yc) <= d*0.62
		return inShaft || inHead
	}
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x, y := float64(px*ss+sx)+0.5, float64(py*ss+sy)+0.5
					if !inRounded(x, y) {
						continue
					}
					tg := (x + y) / (2 * fn)
					col := [3]float64{c1[0] + (c2[0]-c1[0])*tg, c1[1] + (c2[1]-c1[1])*tg, c1[2] + (c2[2]-c1[2])*tg}
					if arrow(x, y, fn*0.38, fn*0.24, fn*0.78, true) || arrow(x, y, fn*0.62, fn*0.22, fn*0.76, false) {
						col = [3]float64{255, 255, 255}
					}
					r, g, b, a = r+col[0], g+col[1], b+col[2], a+1
				}
			}
			if a > 0 {
				k := float64(ss * ss)
				img.SetRGBA(px, py, color.RGBA{uint8(r / k), uint8(g / k), uint8(b / k), uint8(255 * a / k)})
			}
		}
	}
	return img
}

func resize(src image.Image, s int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, s, s))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Over, nil)
	return dst
}

func savePNG(path string, img image.Image) {
	f, err := os.Create(path)
	must(err)
	defer f.Close()
	must(png.Encode(f, img))
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "build:", err)
		os.Exit(1)
	}
}
