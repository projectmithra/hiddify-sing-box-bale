# hiddify-sing-box-bale

Fork of [hiddify-sing-box](https://github.com/hiddify/hiddify-sing-box) with the **Bale protocol mimicry transport** compiled in. Part of [Project Mithra](https://github.com/projectmithra).

The Bale transport makes proxy tunnel traffic indistinguishable from legitimate [Bale messenger](https://bale.ai) sessions at every layer of DPI inspection.


> **This fork is published for:**
> - Community review of the Bale transport implementation and SingBox integration approach
> - Testing by developers and researchers with access to alternative CDN routing paths
> - Reference for the Bale protobuf wire format, handshake sequence, and frame padding - saving other researchers months of reverse engineering
> - Preparation for a future upstream pull request to the Hiddify team when conditions and coordination permit
>
> See [bale-transport](https://github.com/projectmithra/bale-transport) for full protocol documentation, the standalone binary, and current deployment status.

---

## Pre-built Binaries (CI/CD)

Pre-built binaries and APKs are available via GitHub Actions - no local build environment required.

### Download binaries

Go to [Actions → Build sing-box with Bale transport](../../actions/workflows/build-binaries.yml) → click the latest green run → scroll to **Artifacts** and download:

| Artifact | Platform | Use case |
|----------|----------|----------|
| `sing-box-bale-linux-amd64` | Linux x86_64 | Servers, WSL, desktop Linux |
| `sing-box-bale-android-arm64` | Android ARM64 | Termux on Android phones |
| `sing-box-bale-windows-amd64.exe` | Windows x86_64 | Windows desktop |

Binaries are built automatically on every push to the `extended` branch, or on manual dispatch.

### Download Hiddify APK

Go to [Actions → Build Hiddify APK with Bale transport](../../actions/workflows/build-apk.yml) → **Run workflow** → download the `Hiddify-Bale-APK` artifact.

This builds a complete Hiddify Android app with the Bale transport compiled in. Install the APK, import a JSON config with `"type": "bale"` transport, and connect - no Termux or command line needed.

### Trigger a build manually

1. Go to the [Actions tab](../../actions)
2. Select the workflow
3. Click **Run workflow**
4. Download artifacts from the completed run

---

## Manual Building

### Prerequisites

- Go 1.22+
- Git
- [bale-transport](https://github.com/projectmithra/bale-transport) cloned alongside this repo

### Clone and initialize

```bash
git clone --branch extended https://github.com/projectmithra/hiddify-sing-box-bale.git
git clone https://github.com/projectmithra/bale-transport.git
cd hiddify-sing-box-bale
git submodule update --init --recursive
```

### Build for Linux (amd64)

```bash
go build -ldflags="-s -w" -tags "with_gvisor,with_quic,with_utls" -o sing-box-bale ./cmd/sing-box
```

### Build for Android (arm64) — Termux-compatible

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -tags "with_gvisor,with_quic,with_utls" -o sing-box-bale-android ./cmd/sing-box
```

### Known build issues

- **`with_ech` and `with_reality_server` build tags**: Deprecated in newer sing-box versions. Omit from build tags.
- **Submodules**: Always run `git submodule update --init --recursive` after cloning.
- **bale-transport dependency**: The `go.mod` includes a `replace` directive pointing to `../bale-transport`. Clone the [bale-transport](https://github.com/projectmithra/bale-transport) repo alongside this one.

---

## Client Configuration

```json
{
  "dns": {
    "servers": [
      { "address": "udp://8.8.8.8", "detour": "direct" }
    ]
  },
  "inbounds": [
    { "type": "mixed", "listen": "127.0.0.1", "listen_port": 2080 }
  ],
  "outbounds": [
    {
      "type": "vless",
      "server": "your-worker.workers.dev",
      "server_port": 443,
      "uuid": "your-uuid",
      "tls": {
        "enabled": true,
        "server_name": "your-worker.workers.dev",
        "utls": { "enabled": true, "fingerprint": "chrome" }
      },
      "transport": {
        "type": "bale",
        "worker_url": "wss://your-worker.workers.dev/w",
        "origin": "https://web.bale.ai",
        "path": "/w"
      }
    },
    { "type": "direct", "tag": "direct" }
  ]
}
```

## Transport Options

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `worker_url` | string | (from server) | Full WSS URL to the Cloudflare Worker |
| `worker_host` | string | (from URL) | Host header override |
| `origin` | string | `https://web.bale.ai` | Origin header |
| `accept_language` | string | `fa-IR,fa;q=0.9,...` | Accept-Language header |
| `path` | string | `/w` | WebSocket upgrade path |

## What DPI Sees

| Layer | What DPI Sees | Matches Bale? |
|-------|--------------|:---:|
| IP destination | Cloudflare IP shared with Iranian services | Yes |
| TLS handshake | Chrome fingerprint via uTLS | Yes |
| WebSocket path | `/w` | Yes |
| Origin header | `web.bale.ai` | Yes |
| HTTP headers | `X-Bale-Proto: 1`, `Accept-Language: fa-IR` | Yes |
| First WS message | Protobuf HandshakeRequest | Yes |
| Data frames | ClientEnvelope with Bale service names | Yes |
| Frame sizes | Padded to Bale distribution | Yes |
| Keepalive | Ping/Pong every ~25s +/-3s | Yes |

## Deploying the Full Stack

The complete system requires three components. Each has its own repo with deployment instructions:

1. **This repo** — Client binary or Hiddify APK (download from CI or build manually)
2. **[cloudflare-worker](https://github.com/projectmithra/cloudflare-worker)** — Deploy on Cloudflare free tier, edit two config lines
3. **[bale-transport](https://github.com/projectmithra/bale-transport)** — `docker compose up -d` on any VPS for the unwrapper + xray server

## Related Repositories

- [projectmithra/bale-transport](https://github.com/projectmithra/bale-transport) — Core protobuf codec, standalone binary, server unwrapper, Docker deployment
- [projectmithra/cloudflare-worker](https://github.com/projectmithra/cloudflare-worker) — Edge relay with active probing resistance
- [projectmithra/open-ip-lane](https://github.com/projectmithra/open-ip-lane) — CDN IP scanning methodology

## License

This project is licensed under the same terms as the original [hiddify-sing-box](https://github.com/hiddify/hiddify-sing-box).
