# Shuttle

Fast, encrypted, resumable file transfer between your **Windows PC** and your **Mac** over your home network.
Built for big files (2–5 GB+): streamed straight from disk, verified with SHA-256, and it picks up where it left off if Wi-Fi drops or a laptop goes to sleep.

- No cloud, no accounts, no size limits — files go directly PC ⇄ Mac.
- Auto-discovers your other computer; pair once with a 6-digit code.
- End-to-end encrypted (mutual TLS 1.3 with pinned device certificates).
- Resumes interrupted transfers automatically; never overwrites existing files.
- Keeps the computer awake while transferring.
- Windows: right-click any file → **Send to → Shuttle**.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the design.

## Install

Build once (on any OS with Go 1.24+):

```
go run ./tools/build
```

This creates:

| File | For |
|---|---|
| `dist/Shuttle-Windows.exe` | Windows 10/11 — copy anywhere (e.g. `C:\Program Files\Shuttle\`) and double-click |
| `dist/Shuttle-macOS.zip` | macOS 11+ (Apple Silicon **and** Intel) — unzip, drag `Shuttle.app` to Applications |

**Mac, first launch only:** the app isn't notarized by Apple, so run this once in Terminal (or right-click the app → Open):

```
xattr -dr com.apple.quarantine /Applications/Shuttle.app
```

**Firewall prompts:** Windows will ask to allow Shuttle on networks — tick **Private networks** and allow.
macOS may ask to accept incoming connections — click **Allow**.

## Use

1. Open Shuttle on both computers (a browser window opens with the Shuttle UI).
2. On one computer, click **Pair** next to the other device. Check both screens show the same code, then click **Codes match — Pair** on the other one. (One-time.)
3. Click **Choose files…** or **Choose folder…**. Received files land in `Downloads/Shuttle` (change in **Settings**).

Turn on **Settings → Start Shuttle automatically when I log in** on both machines and it is always ready in the background; on Windows you can then just right-click a file → **Send to → Shuttle**.

If the devices don't find each other (some routers/guest networks block discovery), type the other computer's IP — shown at the top of its Shuttle window — into **Connect by IP**.

### Command line

```
shuttle send ~/Movies/big.mov            # to your paired device
shuttle send --to "MacBook Pro" D:\Videos\clip.mp4 D:\Photos
```

On Mac the binary is at `/Applications/Shuttle.app/Contents/MacOS/shuttle`.

## Speed tips

- Wired Ethernet on at least one side roughly doubles Wi-Fi throughput; Wi-Fi 5/6 typically gives 40–120 MB/s, Gigabit Ethernet ~110 MB/s.
- Locally (loopback, same machine) Shuttle sustains ~250 MB/s including encryption and hashing on both ends, so the network is the bottleneck, not Shuttle.

## Files

| | Windows | macOS |
|---|---|---|
| Settings, identity, log | `%AppData%\Shuttle\` | `~/Library/Application Support/Shuttle/` |
| Received files | `%UserProfile%\Downloads\Shuttle` | `~/Downloads/Shuttle` |
| Partial downloads | `Downloads\Shuttle\.shuttle-parts` (auto-cleaned after 7 days) | same |

To reset pairing, quit Shuttle and delete `cert.pem`/`key.pem` and the `peers` in `config.json`.

## Development

```
go test ./...
go vet ./...
# two instances on one machine (bound to localhost, no firewall prompt):
SHUTTLE_HOME=/tmp/a SHUTTLE_BIND=127.0.0.1 go run . run
SHUTTLE_HOME=/tmp/b SHUTTLE_BIND=127.0.0.1 go run . run --peer-port 47830 --ui-port 47831 --discovery-port 47832
```
