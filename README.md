# hiddify-sing-box-bale

Fork of [hiddify-sing-box](https://github.com/hiddify/hiddify-sing-box) with the **Bale protocol mimicry transport** compiled in. Part of [Project Mithra](https://github.com/projectmithra).

The Bale transport makes proxy tunnel traffic indistinguishable from legitimate [Bale messenger](https://bale.ai) sessions at every layer of DPI inspection.


> **This fork is published for:**
> - Community review of the Bale transport implementation and SingBox integration approach
> - Testing by developers and researchers with access to alternative CDN routing paths
> - Reference for the Bale protobuf wire format, handshake sequence, and frame padding — saving other researchers months of reverse engineering
> - Preparation for a future upstream pull request to the Hiddify team when conditions and coordination permit
>
> See [bale-transport](https://github.com/projectmithra/bale-transport) for full protocol documentation, the standalone binary, and current deployment status.

---

## Building

### Prerequisites

- Go 1.22+
- Git
- Android NDK r27+ (for Android builds)

### Clone and initialize submodules

```bash
git clone --branch extended https://github.com/projectmithra/hiddify-sing-box-bale.git
cd hiddify-sing-box-bale
git submodule update --init --recursive
```

### Build for Linux (amd64)

```bash
go build -ldflags="-s -w" -tags "with_gvisor,with_quic,with_utls" -o sing-box-bale ./cmd/sing-box
```

### Build for Android (arm64)

Requires Android NDK. Android 14+ requires PIE executables built with NDK.

```bash
export CC=$ANDROID_NDK_HOME/toolchains/llvm/prebuilt/linux-x86_64/bin/aarch64-linux-android35-clang
CGO_ENABLED=1 GOOS=android GOARCH=arm64 CC=$CC go build -ldflags="-s -w" -tags "with_quic,with_utls" -o sing-box-bale-android ./cmd/sing-box
```

Note: Cross-compiling with `GOOS=linux GOARCH=arm64` produces static binaries that Android 14+ rejects (requires PIE). Always use `GOOS=android` with the NDK toolchain for Android targets.

### Known build issues

- **Psiphon TLS panic**: The `replace/psiphon-tls` submodule causes a `ConnectionState field count mismatch` panic on Go 1.22+. The Psiphon import is commented out in `include/registry.go` as it is not required for the Bale transport.
- **`with_ech` build tag**: Deprecated in newer sing-box versions. Omit it from build tags.
- **Submodules**: Always run `git submodule update --init --recursive` after cloning. Without this, the build fails with missing `replace/psiphon-tls/go.mod`.

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

## Tested Platforms

- Linux amd64 (Debian) — verified end-to-end
- Android arm64 (Termux, Android 16) — verified end-to-end via NDK build

## Related Repositories

- [projectmithra/bale-transport](https://github.com/projectmithra/bale-transport) — Core protobuf codec, standalone binary, Worker, unwrapper
- [projectmithra/open-ip-lane](https://github.com/projectmithra/open-ip-lane) — Scanning methodology
- [projectmithra/cloudflare-worker](https://github.com/projectmithra/cloudflare-worker) — Edge relay
## License

This project is licensed under the same terms as the original [hiddify-sing-box](https://github.com/hiddify/hiddify-sing-box).
