# Shuttle

Fast, encrypted, resumable file transfer between your **Windows PC** and your **Mac** over your home network.
Built for big files (2–5 GB+): streamed straight from disk, verified with SHA-256, and it picks up where it left off if Wi-Fi drops or a laptop goes to sleep.

- No cloud, no accounts, no size limits. Files go directly PC ⇄ Mac.
- A normal app with its own window on both Windows and macOS.
- Finds your other computer automatically; pair once with a 6-digit code.
- End-to-end encrypted (mutual TLS 1.3 with pinned device certificates).
- Resumes interrupted transfers automatically; never overwrites existing files.
- Keeps the computer awake while transferring.
- Windows: right-click any file → **Send to → Shuttle**.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the design.

## Install

Download both from the **[Releases page](https://github.com/zainulabedeen123/Shuttle/releases/latest)**:

| File | For |
|---|---|
| `Shuttle-Windows.exe` | Windows 10/11. Put it somewhere permanent (e.g. `%LocalAppData%\Programs\Shuttle`) and double-click. |
| `Shuttle-macOS.zip` | macOS 11+ (Apple Silicon **and** Intel). Unzip and drag `Shuttle.app` to Applications. |

**Mac, first launch only:** the app isn't notarized by Apple, so run this once in Terminal (or right-click the app → Open):

```
xattr -dr com.apple.quarantine /Applications/Shuttle.app
```

**Permissions (important):**

- **Mac:** when macOS asks *"Allow Shuttle to find devices on local networks?"*, click **Allow**. If you missed it: System Settings → Privacy & Security → **Local Network** → turn on Shuttle. If the Mac firewall is on, allow Shuttle under System Settings → Network → Firewall → Options.
- **Windows:** when Windows Firewall asks, allow Shuttle on **both Private and Public** networks (home Wi-Fi is often marked Public).

Shuttle shows a warning banner inside the app if it detects the firewall blocking it.

## Use

Shuttle is a normal app with its own window. Behind it, a small background service does the actual sending and receiving, so **closing the window doesn't stop transfers**, and your other computer can still send you files. Use **Quit** inside the app to stop it completely.

1. Open Shuttle on both computers. Each one finds the other automatically within a few seconds: it listens for broadcasts *and* scans the local network, so it works even on routers that block discovery.
2. Click **Pair** next to the other computer, check both screens show the same 6-digit code, and confirm on the other one. (One-time.)
3. Click **Choose files…** or **Choose folder…**. Received files land in `Downloads/Shuttle` (change in **Settings**).

Turn on **Settings → Start Shuttle automatically when I log in** on both machines and they are always ready to receive.

### Troubleshooting: "connection timed out" or the other computer never appears

1. Both computers must be on the **same Wi-Fi network** (not one on a guest network or a phone hotspot).
2. On the Mac: the Local Network permission and the firewall, as above.
3. On Windows: the firewall must allow Shuttle for the current network type.
4. Try **Connect by IP** using the IP shown at the top of the *other* computer's Shuttle window. IP addresses can change after a reboot.

### Command line

```
shuttle send ~/Movies/big.mov            # to your paired device
shuttle send --to "MacBook Pro" D:\Videos\clip.mp4 D:\Photos
```

On Mac the binary is at `/Applications/Shuttle.app/Contents/MacOS/shuttle`.

## Speed tips

- Wired Ethernet on at least one side roughly doubles Wi-Fi throughput. Wi-Fi 5/6 typically gives 40–120 MB/s; Gigabit Ethernet ~110 MB/s.
- Locally (same machine) Shuttle sustains ~250 MB/s including encryption and hashing on both ends, so the network is the bottleneck, not Shuttle.

## Files

| | Windows | macOS |
|---|---|---|
| Settings, identity, log | `%AppData%\Shuttle\` | `~/Library/Application Support/Shuttle/` |
| Received files | `%UserProfile%\Downloads\Shuttle` | `~/Downloads/Shuttle` |
| Partial downloads | `Downloads\Shuttle\.shuttle-parts` (auto-cleaned after 7 days) | same |

## Building

Releases are built by GitHub Actions ([.github/workflows/release.yml](.github/workflows/release.yml)): push a tag like `v1.2.0` and it publishes a Release with both downloads. The macOS app has to be compiled on a Mac, because its native window uses Apple's WebKit via cgo.

Locally:

```
go run ./tools/build windows   # dist/Shuttle-Windows.exe (from any OS)
go run ./tools/build mac       # dist/Shuttle-macOS.zip   (on macOS only)
go test ./... && go vet ./...
```

Two instances on one machine for testing:

```
SHUTTLE_HOME=/tmp/a go run . run
SHUTTLE_HOME=/tmp/b go run . run --peer-port 47830 --ui-port 47831 --discovery-port 47832
```
