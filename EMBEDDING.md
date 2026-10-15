# Embedding the client (`arikan-library` branch)

This branch is upstream **v1.8.3** plus the changes Arikan's Android app needs to run the
client **inside its own process**, one client per node, instead of shipping it as a separate
program. Upstream is untouched otherwise: the same files, the same logic, the same flags.

Base: tag `v1.8.3` (`e8a9696`). Branch: `arikan-library`, tag `v1.8.3-arikan.1`.

## What changed

**The `client` package is a library, not a program.** `package main` → `package vkclient`,
and `func main()` moved to `cmd/client/main.go`, which is a faithful port of it: same flags,
same help text, same signal handling. `go build ./cmd/client` gives the same command line
client as upstream, so the two can be run side by side and diffed.

**New: `client/library.go`** — `Config`, `Run(ctx, cfg)`, `Instance`, `Event`.

* Everything that `main()` read from flags is a `Config` field.
* `Run` returns errors where `main()` panicked (`Need peer address!`, `failed to listen`,
  the VLESS TCP listen). An embedding host cannot afford a panic: this code runs in the
  process that also carries the user's tunnel.
* Per-client state moved off the package: the local peer address (who to send replies to),
  the connected-stream counter, the credentials cache, and the cancel func. `activeLocalPeer`
  and `connectedStreams` as globals were correct for exactly one client; a second node in the
  same process would have answered the first node's WireGuard peer and counted its streams.
* `globalAppCancel` became `Instance.requestStop`: a fatal captcha on one node stops that
  node's client, not its neighbour's.
* **Stream IDs carry the client.** `Config.StreamIDBase` (`StreamsPerInstance` = 100 per
  client) offsets every stream number, and `instanceForStream(streamID)` is how the code finds
  the owning client. That is why the diff touches so little of the 900-line auth/captcha path:
  the functions there already had a stream ID, so no signatures had to change. It also keeps
  the log readable with several clients running — a line's `[STREAM n]` tag names its client.
* `Config.OnEvent` reports `listening` / `established` / `closed` / `retry` / `captcha` /
  `fatal`, which is what a host shows as "works / doesn't".
* Left process-wide on purpose: `globalCaptchaLockout` and `handshakeSem` (they protect the
  device from VK's rate limits, not one node), plus `isDebug`, `manualCaptcha` and
  `autoCaptchaSliderPOC` — a host must pass the same values to all its clients.

**New: `client/socketpolicy.go`** — `SocketPolicy`, `SetSocketPolicy`.

The client opens sockets for two different purposes: the loopback socket the app side talks
to, and the ones reaching the outside (VK's API and call link, DNS, the TURN server). A host
that runs its own tunnel has to claim the second kind, or that traffic is routed into the
tunnel the client is feeding — a loop, and on Android a silently broken one. `SocketPolicy.Control`
is called on the raw socket before connect, which is where `VpnService.protect` goes.

The outward sockets are: the tls-client's dialer (VK API), the WebSocket to the call link,
the dnsdialer or a host-marked dialer for the captcha page, and pion/turn's transport. All of
them now go through the policy. With no policy set the behaviour is upstream's — the
command-line client is unchanged, public resolvers included.

**New: `client/hooks.go`** — `SetBrowserOpener`, `SetCaptchaResolvedHook`.

Manual captcha solving prints "open this URL in your browser" and shells out to
`xdg-open`/`open`/`am`. A host with a WebView wants the URL instead; the opener receives it
with the stream ID of the client that needs it (the URL always points at the client's own
page on `127.0.0.1:8765`, so a WebView in another process can load it).

**`cmd/client`** keeps `-no-dtls` refusing to run, exactly as upstream: the flag exists, the
mode does not.

## Known limits

* One manual captcha session at a time per process: the captcha page is served on a fixed
  `127.0.0.1:8765`. A second client that needs a human while the first is waiting fails that
  attempt, backs off 60 s and retries — by then the first is usually solved.
* `isDebug` / `manualCaptcha` / `autoCaptchaSliderPOC` are still process-wide, so several
  clients in one process share them. Hosts must pass the same values to every client.

## Keeping up with upstream

```
git fetch --tags upstream
git checkout -b arikan-library-v1.9.0 upstream/v1.9.0   # or rebase
# re-apply: package rename, main() → cmd/client, the Instance moves, the policy call sites
git diff v1.8.3..arikan-library > arikan.patch           # what to re-apply
```

The changes are deliberately mechanical and local: package clause, `main()` out, six global
references to `instanceForStream(...)`, the dial/listen call sites, and the captcha opener.
Nothing in the auth, captcha-solving or TURN protocol code was rewritten.
