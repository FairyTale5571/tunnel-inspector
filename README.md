# tunnel-inspector

Inspect, edit and replay the HTTP requests coming through a Cloudflare Tunnel —
the thing ngrok gives you for free at `localhost:4040`, and `cloudflared` doesn't
give you at all.

```
  internet ──▶ Cloudflare ──▶ cloudflared ──▶ tunnel-inspector ──▶ your app
                                                  :4010              :3000
                                                    │
                                                    └─▶ web UI on 127.0.0.1:4041
```

A single Go binary, nothing outside the standard library, **~12 MB of RAM** at
rest. It sits between `cloudflared` and your app and records what passes through,
without changing a byte of it.

## Why this exists

`ngrok http 3000` is both the tunnel *and* the inspector in one process — traffic
physically flows through it, so showing you the requests costs it nothing.
`cloudflared` is only a tunnel: it hands traffic to your app and shows you
nothing. To see the requests, something has to sit in the middle. That's this.

## What you get

- **A live list of requests** — method, path, status, duration, response size.
- **Full detail** — request and response headers and bodies. `gzip` and
  `deflate` responses are decompressed for display.
- **Edit and replay** — change the method, path, headers or body and send it
  again. Replays are tagged so you can tell them apart.
- **Pretty-printed JSON**, with `{ } Format` / `Minify` buttons. The formatter
  only moves whitespace *outside* strings: `JSON.parse` + `JSON.stringify` would
  silently destroy integers above 2^53 (hello, Telegram channel IDs) and turn
  `1.50` into `1.5`. Minifying returns the original bytes exactly.
- **Failures are recorded too.** If your app is down the proxy answers `502` but
  still stores the request — so you can start the app and replay that webhook
  you just missed.
- **Transparent pass-through** — WebSocket upgrades, Server-Sent Events, chunked
  bodies, the original `Host`, duplicate headers, the raw query string and the
  `X-Forwarded-*` / `CF-*` headers all reach your app unmodified. Your app sees
  exactly what it saw before the inspector existed.

## Install (macOS)

```bash
git clone https://github.com/FairyTale5571/tunnel-inspector.git
cd tunnel-inspector
./install.sh
```

That builds the binary and installs a LaunchAgent which starts at login and is
restarted by `launchd` within a second if it ever dies.

Custom ports — proxy, app, UI, in that order:

```bash
./install.sh 4010 3000 4041
```

### The one thing you have to do by hand

Point your tunnel at the inspector instead of at your app. In **Cloudflare Zero
Trust → Networks → Tunnels → your tunnel → Public hostname**, change the service
from `http://localhost:3000` to `http://localhost:4010`.

**Your app stays on port 3000.** You never touch the app, and you never touch
this setting again.

If your tunnel is configured by a local `config.yml` instead of the dashboard,
change `service:` there and restart `cloudflared`.

Any of `localhost`, `127.0.0.1` or `[::1]` works, because the inspector listens
on both loopback families. This matters: `cloudflared` resolves `localhost` to
`[::1]` and does **not** fall back to IPv4, so a single-family bind would 502
for no visible reason.

## Use it

Open **http://127.0.0.1:4041**.

| Control | What it does |
|---|---|
| Click a row | Opens headers and bodies for that request |
| `Pause` | Stops the list auto-refreshing while you read |
| `Clear` | Wipes the history, in memory and on disk |
| `{ } Format` | Indents the JSON in the body editor |
| `Minify` | Collapses it back to a single line |
| `Send` | Replays the request as edited, straight to your app |

Whatever is in the body field is what gets sent, byte for byte, and
`Content-Length` is recalculated for you. If the receiver verifies a signature
over the raw body (Stripe, GitHub), press `Minify` before sending — whitespace
changes the bytes and the signature won't match.

## Run it manually

Useful for a one-off, or to watch the log on stdout:

```bash
go build -o tunnel-inspector . && ./tunnel-inspector
```

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-listen` | `TI_LISTEN` | `localhost:4010` | where the tunnel connects |
| `-upstream` | `TI_UPSTREAM` | `http://localhost:3000` | your app |
| `-ui` | `TI_UI` | `localhost:4041` | the web interface |
| `-history` | `TI_HISTORY` | `~/.tunnel-inspector/history.jsonl` | stored requests |

Nothing is ever exposed to your network: `localhost` and an explicit loopback IP
both bind the loopback interface only.

## Managing the service

```bash
launchctl kickstart -k gui/$UID/io.github.fairytale5571.tunnel-inspector
```

```bash
launchctl print gui/$UID/io.github.fairytale5571.tunnel-inspector
```

```bash
./uninstall.sh
```

Logs: `/tmp/tunnel-inspector.out` and `/tmp/tunnel-inspector.err`.

After editing the source, `./install.sh` rebuilds and restarts in one step.

## Memory, deliberately

Captured traffic is untrusted and unbounded, so the limits are hard ones:

- bodies are stored up to **32 KB**; past that only the size is kept;
- binary bodies (images, video, archives) are never copied at all;
- a ring buffer of the **last 100 requests**, evicted entries released at once;
- history is appended one JSON line per request — the file is rewritten only
  during compaction, every 200 lines.

| State | RSS |
|---|---|
| idle / ordinary traffic | **~12 MB** |
| worst case: 100 consecutive requests with 40 KB text bodies | **~21 MB** |

To trade memory for fidelity, raise `bodyCap` and `maxEntries` at the top of
`store.go`.

## Security

The captured history holds real cookies and `Authorization` headers from live
traffic. Accordingly:

- both listeners bind **loopback only**; requests from any other address are
  refused;
- every route, reads included, rejects a `Host` that isn't loopback, so a
  DNS-rebinding page cannot read your history;
- replay and clear additionally require a loopback `Origin`, so a random browser
  tab cannot fire requests at your app;
- the history file is `0600` inside a `0700` directory;
- the UI renders every captured value with `textContent` under a restrictive
  CSP — nothing from the wire is ever interpolated as HTML.

## Known limits

- **Login, not boot.** A LaunchAgent starts when you log in, while `cloudflared`
  usually starts at boot. Between boot and login the tunnel returns 502. Install
  it as a `LaunchDaemon` if that matters to you.
- **It is in the path now.** If it crashes the tunnel 502s until `launchd`
  brings it back — well under a second, but not zero.
- **Streams appear when they finish.** SSE and long-poll requests show up only
  once the stream closes; until then there is no duration or size to report.
- **`br` and `zstd` responses** are shown compressed. Replay the request without
  `Accept-Encoding` to read the body.

## Layout

| File | What is in it |
|---|---|
| `main.go` | startup, flags, the reverse proxy, request/response capture |
| `store.go` | ring buffer, append-only history, compaction, body limits |
| `api.go` | UI endpoints, replay, access control, decompression for display |
| `ui.go` | the whole web interface — HTML, CSS, JS, the JSON formatter |
| `install.sh`, `uninstall.sh` | LaunchAgent setup and removal |

About 1400 lines of Go, standard library only.

## License

MIT
