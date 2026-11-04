# Shuttle — Windows ⇄ Mac file transfer

## 1. Goals

| Requirement | Decision |
|---|---|
| Works on Windows and macOS | One Go codebase → single static binary per OS (no runtime, no installer, no cgo) |
| Large files (2–5 GB) | Streaming only (never buffered in RAM), 1 MiB I/O buffers, resumable, end-to-end SHA-256 verification |
| "Seamless" | Auto-discovery on LAN, one-time pairing, auto-accept from paired devices, browser UI, Windows "Send to" menu, start at login, keeps machine awake during transfers |
| Safe | Mutual TLS with pinned certificates, 6-digit pairing code check, path sanitisation, local UI locked to 127.0.0.1 + per-session token |

Non-goals (v1): transfers over the internet / NAT traversal, continuous folder sync, mobile clients.

## 2. High-level architecture

```
 ┌──────────────── Windows PC ────────────────┐          ┌──────────────── MacBook ───────────────────┐
 │  Browser UI  ── http://127.0.0.1:47821 ──┐ │          │ ┌── http://127.0.0.1:47821 ──  Browser UI   │
 │  CLI / "Send to" ─────── local API ──────┤ │          │ ├────── local API ─────── CLI              │
 │                                          ▼ │          │ ▼                                          │
 │                         ┌──────────────────┐   mTLS   ┌──────────────────┐                        │
 │                         │   Shuttle core   │◀────────▶│   Shuttle core   │                        │
 │                         │  transfer engine │ TCP 47820│  transfer engine │                        │
 │                         └──────────────────┘          └──────────────────┘                        │
 │                               ▲  UDP 47822 broadcast discovery  ▲                                  │
 └───────────────────────────────┴─────────────────────────────────┴──────────────────────────────────┘
```

Every device runs the same program and is both **sender and receiver** (symmetric peers, no server).

### Components (`internal/app`)

| File | Responsibility |
|---|---|
| `config.go` | Config dir (`%AppData%\Shuttle`, `~/Library/Application Support/Shuttle`), device ID/name, download folder, paired peers |
| `identity.go` | Generates a per-device ECDSA P-256 self-signed cert on first run; SHA-256 fingerprint is the device identity |
| `discovery.go` | UDP broadcast announce every 3 s + listener; tracks online devices and updates peer IPs |
| `peerserver.go` | TLS server on :47820 — pairing, offer, upload, status, commit endpoints |
| `peerclient.go` | TLS client with certificate pinning |
| `sender.go` | Outgoing transfer jobs: walk files/folders, stream, retry/resume, verify |
| `receiver.go` | Incoming transfer state: `.part` files, hashing, commit/rename, conflict naming |
| `uiserver.go` + `web/` | Local web UI + JSON API + Server-Sent Events for live progress |
| `events.go` | Pub/sub hub for progress events |

`internal/platform` holds OS-specific bits: native file/folder pickers (PowerShell / AppleScript), open browser/folder, free disk space, keep-awake (`SetThreadExecutionState` / `caffeinate`), start-at-login, Windows "Send to" shortcut, console attach.

## 3. Security model

1. **Identity** — each device has a self-signed cert. Fingerprint = SHA-256 of DER cert.
2. **Pairing** (one time):
   - A connects to B over TLS (`POST /v1/pair`), presenting its cert. B's cert fingerprint is captured by A.
   - Both sides compute `code = SHA256(sort(fpA, fpB))` → 6 digits, shown on both screens.
   - B's user clicks **Accept** after confirming the codes match → B stores A; A stores B.
   - A man-in-the-middle would produce different fingerprints, so codes would not match.
3. **All other requests** use mutual TLS: server requires a client cert whose fingerprint is in the paired list; client verifies the server cert matches the pinned fingerprint. All file data is encrypted (TLS 1.3, AES-GCM — hardware accelerated, multi-GB/s).
4. **Local UI** binds to `127.0.0.1` only, rejects foreign `Host` headers (DNS rebinding) and requires a random per-run token on every API call (CSRF). The CLI reads that token from a file in the config dir.
5. **Receiver path safety** — relative paths are cleaned; absolute paths, `..`, drive letters, and reserved names are rejected. Existing files are never overwritten (`name (1).ext`).

## 4. Transfer protocol (HTTP/1.1 over mTLS)

```
Sender                                         Receiver
  │ POST /v1/offer {transferId, files[]} ───────▶ │  check free space, create state
  │ ◀──────────────── {offsets[]}  (bytes already received per file, for resume)
  │ for each file:
  │   PUT /v1/upload/{tid}/{i}?offset=N  ───────▶ │  append to <dl>/.shuttle-parts/{tid}-{i}.part
  │       (streamed body, 1 MiB chunks)           │  hash = SHA256(existing prefix + stream)
  │ ◀──────────────── {size, sha256}
  │   compare with sender's own SHA-256
  │   POST /v1/commit/{tid}/{i} {sha256} ───────▶ │  verify, rename to final unique name
  │ ◀──────────────── {savedAs}
  │   (on mismatch: POST /v1/discard, retry from 0)
```

- **transferId** = hash(senderId + absolute path + size + mtime) → re-sending the same file after a crash, sleep or Wi-Fi drop resumes where it left off.
- **Resume**: on any network error the sender retries with backoff (up to 10 attempts), asking `GET /v1/status/{tid}/{i}` for the current offset. On resume both sides re-hash the existing prefix locally (fast disk read) so verification still covers the whole file.
- **Fast resume**: when an upload is interrupted while the receiver keeps running (the usual Wi-Fi drop), the receiver keeps its SHA-256 state in memory at the exact byte offset, so resuming doesn't re-read the partial file.
- **Throughput**: single TCP stream per file saturates Gigabit Ethernet / Wi-Fi 6 on LAN. 1 MiB buffers, no per-chunk round trips. Part files older than 7 days are cleaned at startup.

## 5. Discovery

Two mechanisms run side by side, so devices appear automatically even when one is blocked:

1. **UDP broadcast** on port 47822 (to `255.255.255.255` and every interface's directed broadcast) with
   `{app, v, id, name, os, port, fp}`. Receivers reply unicast, so new devices appear instantly when the network allows it.
2. **TCP scan** (`scan.go`): every 30 s while a window is open (every 2 min otherwise), the service tries Shuttle's port on every address of each local /24 subnet (~2 s) and asks any responder who it is (`GET /v1/hello`). This finds devices on routers that drop broadcasts.

Fallback: **Connect by IP**. Paired peers remember their last IP and are probed directly.

Platform requirements that affect discovery:
- The macOS bundle declares `NSLocalNetworkUsageDescription`; without it, macOS 15+ silently denies local-network access.
- A firewall check (macOS `socketfilterfw`, Windows Defender Firewall rules) shows a warning banner in the app when Shuttle is blocked.

## 6. User experience

Shuttle is one executable in two roles:
- **Background service** (`shuttle run`): discovery, transfers, local UI server. Started at login or by the app.
- **App window** (`shuttle` with no arguments): a native window (WebView2 on Windows, WebKit on macOS) showing the service's UI. Closing it leaves the service receiving; an incoming pairing request opens it automatically.

- Launching the app while it's already running just opens another window onto the same service.
- Devices panel: online devices, **Pair**, pairing code confirmation, **Connect by IP**.
- Send panel: pick a device, **Choose files** / **Choose folder** (native OS dialogs), live progress, speed, ETA, cancel.
- Received panel: history, **Show in folder**.
- Settings: device name, download folder, start at login.
- CLI: `shuttle send <paths...> [--to <device>]`; Windows **Send to → Shuttle** right-click menu.

## 7. Ports & firewall

| Port | Proto | Purpose |
|---|---|---|
| 47820 | TCP | peer transfers (mTLS) |
| 47821 | TCP | local UI, 127.0.0.1 only |
| 47822 | UDP | discovery |

Windows asks once to allow network access (allow both **Private** and **Public**: home Wi-Fi is often marked Public). macOS asks for Local Network access and, if its firewall is on, to accept incoming connections.

## 8. Build

```
build.ps1 / build.sh  →  dist/shuttle-windows-amd64.exe
go run ./tools/build mac      →  dist/Shuttle-macOS.zip   (macOS only; native window needs cgo)
GitHub Actions builds both on every v* tag and publishes a Release.
```

## 9. Implementation phases

1. Config, identity, peer TLS server/client, pairing.
2. Transfer engine: offer/upload/status/commit, resume, verification.
3. Discovery.
4. Local UI + SSE + CLI.
5. Platform integrations (dialogs, keep-awake, autostart, Send to).
6. Cross-compile, end-to-end test with multi-GB file between two local instances.
