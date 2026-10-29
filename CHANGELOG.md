# Changelog

## 0.11

### Added — distributed load testing

- **Master / agent split.** `-master 0.0.0.0:8443` runs the control plane and a web
  console on one TLS port; `-join wb1_...` turns any other host into a load generator.
  The master splits the requested total concurrency across the selected nodes
  (`ceil(total / nodes)`, clamped to 1..256 each) and streams every node's progress into
  one live view with an aggregate RPS chart.
- **Agents never listen.** They dial the master, long-poll for start/stop commands and
  push a sample every second, so a generator can live in the most restricted segment
  and needs no inbound rule. A task is limited to 64 nodes, 256 connections per node
  and 24 hours.
- **Rates come from counters, not clocks.** Nodes report cumulative
  requests/failed/bytes; the master derives per-node and aggregate rates from
  successive samples, so clock skew between machines cannot fabricate a throughput
  number. Reported peak RPS is never below the average.
- **Task states say what happened.** `done` (ran to its duration), `stopped` (console
  stop), `degraded` (a node stopped reporting). A node that misses the deadline is
  marked lost after a grace period instead of leaving the task running forever.
- Finished tasks are written to `<data>/tasks/<id>.json` and shown in the console
  history after a restart.

### Added — control-plane security

- **Per-install CA and mutual TLS.** The master generates an ECDSA P-256 CA on first
  start and issues each agent its own client certificate. Nothing is trusted from the
  system root store, and TLS 1.3 is the only accepted protocol.
- **One-time join tokens.** `-join` carries the master URL, an HMAC-SHA256 signed token
  (expiry, default 30 minutes, single use via a recorded nonce) and the CA's SHA-256
  fingerprint. The agent verifies that the presented chain root matches that
  fingerprint *and* that the leaf was signed by it, so first contact cannot be
  intercepted. A reused token, an expired token and a token signed by another master's
  CA are all refused.
- **Revocation that actually bites.** Revoking a node makes the control plane reject its
  certificate immediately, and the registered serial is re-checked on every call, so a
  superseded certificate stops working the moment it is renewed. Agents renew
  themselves during the last 30 days of validity.
- **Console hardening.** bcrypt password hash, HttpOnly + Secure + SameSite=Strict
  session cookie, a CSRF header on every state-changing request, per-IP login
  throttling with exponential lockout after 5 failures, a CSP with no inline script,
  and an append-only JSONL audit log covering enrolment, rejected enrolments, token
  issuance, revocation, logins and task lifecycle.
- The console (HTML/CSS/JS) is embedded in the binary, so deployment is still one file.

### Fixed — issues found while building distributed mode

- **`fake-useragent` data race on the request path.** `browser.Android()` reads a
  shared, unlocked `*rand.Rand`, so any run with more than one worker raced on it
  (`go test -race` reproduces it; 0.10 and earlier are affected too). User agents are
  now drawn from a pool built once at startup and the hot path no longer touches that
  source.
- **The master could not start with a fresh `-data` directory.** `admin.hash` was
  written before the directory existed, so the first run died with
  `open <dir>/.tmp-...: The system cannot find the path specified`.
- **A task that ran its full duration was reported as `stopped`.** The master answers
  "stop" when the deadline passes and the agent treated that as a manual interruption.
  The progress reply now carries the reason (`deadline` vs `stopped`), and only the
  latter marks a task stopped.
- **Peak RPS was 0 on short tasks.** With one or two samples the master never got a
  second counter to diff. The agent now closes each run with one final sample, and the
  reported peak is never below the average.

## 0.10

Everything below was reproduced against the previous build before being fixed, and is
covered by regression tests in `*_test.go`.

### Fixed — benchmark produced no traffic

- **`-s` without `-d` sent zero requests and spun at 100% CPU.** The target URL was only
  assigned inside the `-d` and `-sub` branches, so a plain `webBenchmark -s <url>` ran
  with an empty target, failed `http.NewRequest` on every iteration and busy-looped.
  Measured before: 0 requests and 1.06 CPU seconds in 5 seconds. Measured after: 10114
  requests and 0.06 CPU seconds in 4 seconds. The target is now always initialised from
  `-s`, and a missing target exits with a clear message instead of spinning.
- **`-i` forced every connection onto port 80.** The dial hook compared `host:port`
  against the `"http"` / `"https"` prefixes, so neither branch could ever match and the
  port was always rewritten to `80`. HTTPS targets and any non standard port were
  therefore unreachable. The port now comes from `net.SplitHostPort` on the original
  address.
- **`-i` broke TLS handshakes.** `DialTLSContext` was set but returned a plain TCP
  connection, which Go treats as an already-negotiated TLS connection. The hook is gone;
  `DialContext` plus `TLSClientConfig` covers both schemes.

### Fixed — features silently doing nothing

- **`-H 'RandomN:value'` never randomised the header name.** The length was parsed from
  the value instead of the key, so `-H Random10:abc` literally sent `Random10: abc`.
- **`-f` (on by default) stalled the whole run and then sent empty headers.** The country
  CIDR list was fetched on the request path whenever it was missing, so an unreachable
  CDN blocked every routine, and a failed download was retried on every request while
  sending `X-Forwarded-For: ""`. The list is now preloaded in the background, requests
  never touch the network for it, a failure falls back to a fully random address, and the
  warning is printed once.
- **Multiple `-i` addresses collapsed to one per second.** `rand.Seed(time.Now().Unix())`
  reseeded the global source on every request, so every request inside the same second
  picked the same address. All seeding calls are gone.
- **`-d` printed a pointer and could lose the location.** It logged the global `*string`
  flag (`Headers for 0x...`), indexed `Header["Location"][0]` without checking the key,
  and followed redirects — so an unreachable destination discarded the location. It now
  makes a single request, reads the first hop's `Location`, resolves relative values, and
  never adds traffic to the real target.

### Fixed — crashes

- **Status bar could kill the process.** Two NIC snapshots were aligned by slice index;
  adding or removing an interface (VPN connect/disconnect) made the lookup index out of
  range. Counters are now keyed by interface name. `readableBytes` also clamps its unit
  index instead of panicking on negative, NaN or infinite input.
- **Malformed CIDR entries crashed the IP generator.** `net.ParseCIDR` errors were
  ignored, so an empty line or comment produced a nil dereference. Parsing now validates
  and skips bad lines, and returns an error for invalid input.
- **`-rps` accepted negative sizes**, which panicked in `make([]byte, n)` and was then
  swallowed by a recover loop that restarted the routine. Invalid values are rejected at
  startup.

### Fixed — measurement fidelity

- **The address family was left entirely to the system resolver.** Go prefers IPv6 on a
  dual stack host, and a family can connect fine while being throttled, so nothing falls
  back. Measured on one VPS against the same Cloudflare endpoint: IPv6 delivered 70 KB/s
  and IPv4 delivered 143 MB/s, a difference of about 2000x, and the tool reported the
  wrong one by default. The dialer now resolves IPv4 only unless `-6` is given, and stops
  with an actionable error when the requested family has no address.
- **Every request carried a hardcoded 10 second timeout, which caps large downloads on
  slow links.** Aborting a body mid-flight discards the progress and forces a new TCP and
  TLS handshake, so the connection never leaves slow start. Pulling a 1.6 MB asset over a
  ~7 Mbit international link, the tool reported 267 KB/s while eight parallel `curl`
  processes on the same host sustained 894 KB/s. The timeout is now `-t` (default 30
  seconds, `0` disables it).
- **A body cut short by the timeout still counted as a successful request.** The error
  from reading the body was discarded, so `Requests` grew while `Loaded` stayed low and
  the shortfall looked like a slow server. A request now counts as completed only after
  its body has been read to the end; truncated bodies land in `Failed`.
- **A run that produced no traffic gave no reason for it.** Errors only reached a counter,
  so `-6` against an IPv4 only host looked like a silent stall. The reason now appears as
  `LastError` in the status bar and is printed once on stderr; requests cancelled by
  `Ctrl+C` are excluded so shutdown does not look like a failure.
- **A new `Transport` was created for every request**, defeating the connection pool
  entirely: each request performed a fresh TCP (and TLS) handshake and idle connections
  were never reclaimed. 10114 requests now reuse 2 connections. `-nr` restores the
  one-connection-per-request behaviour explicitly.
- **Status bar showed whole DNS records instead of addresses** (`1.2.3.4.\t3600\tIN\tA\t...`)
  and `nslookup` wrote to stdout, corrupting the live UI. It now returns plain addresses
  and stays silent.
- **`/20` and other non octet aligned CIDR blocks were sampled as if they were `/24`.**
  The host part is now randomised per bit using the mask, so the whole block is covered.
- **Empty `X-Forwarded-For` headers were still sent** when address generation failed.
- **Status bar flickered** because Go map iteration order is random; interfaces are now
  sorted by name.

### Added

- **`-proto http1|http2|http3` and a startup capability probe.** The probe makes one plain
  request and reports the negotiated ALPN plus the `Alt-Svc` header, which is what actually
  answers "does this server do HTTP/3":
  `alpn=h2 alt-svc=none -> server supports http2 only`, or
  `alpn=h2 alt-svc=h3=":443"; ma=86400 -> server advertises http3`. HTTP/3 is never
  negotiated through ALPN, so a missing `Alt-Svc` is the answer, not a probe failure.
- **HTTP/2 support.** Go silently disables h2 as soon as a custom `TLSClientConfig` or
  `DialContext` is set — which this tool needs — so `-proto http2` sets
  `ForceAttemptHTTP2`. It is opt in because it changes what is being measured: on a target
  that caps each client address, eight h1 connections reached 508 KB/s while h2 reached
  460 KB/s.
- **HTTP/3 over QUIC** via `quic-go`, including `-i` and `-4`/`-6` through a QUIC dial hook.
  Validated against a server that advertises h3: 221 requests completed over QUIC. Against
  a server that does not, the probe now warns up front instead of leaving the user staring
  at timeouts.
- **A 1xx response is no longer counted as a completed request.** The h3 client surfaces
  `103 Early Hints` as the final response, which made a real run report 25 successful
  requests with 0 bytes loaded. It now counts as a failure with a clear reason.
- **`-px` — spoof the full set of proxy headers.** Every request carries a fresh forged
  client address in `X-Forwarded-For`, `X-Real-IP`, `X-Client-IP`, `True-Client-IP`,
  `CF-Connecting-IP`, `Fastly-Client-IP`, `X-Cluster-Client-IP`, `X-Originating-IP`,
  `X-Remote-IP`, `X-Remote-Addr`, `X-Proxy-User-Ip`, `X-Original-For` and the RFC 7239
  `Forwarded` header, with `X-Forwarded-Proto` / `X-Forwarded-Host` kept consistent with
  the real request so the edge does not reject it outright. It answers one question: do
  my rate limits key on the address the proxy reports, or on the TCP source? Measured on a
  directly reachable host: 501.9 KB/s without, 502.5 KB/s with — the limits were keyed on
  the TCP source, so the feature correctly showed that no bypass exists.
- **Auto ramp when `-c` is omitted.** Starts at 4 connections, measures 4 second windows
  and doubles while throughput beats the best result so far by more than 10%, then settles
  on the concurrency that produced that best result. A resizable gate lets it step back
  down, so an overshooting probe does not leave the run on a worse setting; windows
  containing failures never count as an improvement. Measured on a long haul link the ramp
  settled at 8 connections / 568 KB/s after the naive "last tried" version had walked up
  to 64 and stayed at 400 KB/s.
- **Live concurrency keys in auto mode.** `1`/`2`/`3`/`4` raise the concurrency by 10%,
  20%, 30% or 2x; `5`/`6`/`7`/`8` lower it by the same amounts. The first press takes
  control (phase becomes `manual`, automatic ramping stops) and later presses keep
  applying, so you can search by hand without restarting the run. Requires a real
  terminal; with redirected stdin the `Keys:` line is simply omitted. Measured on a real
  pty: 8 → +20% → 10 → x2 → 20 → −30% → 14.
- **TLS session resumption.** `tls.Config` defaults `ClientSessionCache` to nil, so every
  connection paid a full handshake. The cache is now shared across all workers, which
  matters most while the auto ramp is opening connections: at 128 concurrent connections on
  a ~1 s RTT path the measured rate went from 123 KB/s to 146 KB/s.
- **Resolution cache for dialling.** `dialFamily` queried the resolver on every new
  connection; results are now cached for 60 s per host.
- **The ramp table shows the rate per connection.** When it halves every time the
  concurrency doubles while the total stays flat, the target is capping you per client
  address rather than the tool leaving capacity unused.
- `-nr` — open a new TCP connection per request.
- `-t` — per-request timeout in seconds (default 30, `0` disables it).
- `-4` / `-6` — pin the address family. IPv4 is the default; `-6` is an explicit opt in.
- Status bar fields `Requests`, `Failed`, `Loaded`, `Avg` and `Elapsed`, plus a `LastError`
  line; each distinct request error is also printed once on stderr.
- `-fc` — country code for the `X-Forwarded-For` address pool (default `cn`).
- `-dns` — DNS server used for the target address display (default `8.8.8.8`).
- Regression tests covering every item above; `go test -race ./...` is clean.
- `-h` output now documents the flags with working examples.

### Changed

- **Releases are built by a GitHub Actions workflow from a version tag.** It builds all
  four targets in one job, attaches them with `SHA256SUMS`, and uses the matching
  changelog section as the release notes. There is no PR or branch trigger: formatting,
  vet, tests, race, the four cross builds and the documentation checks all run locally via
  `scripts/preflight.ps1` before a tag is pushed. The workflow still refuses to publish if
  the tag, `const version` in `main.go`, and the changelog section disagree.
- Adds `github.com/quic-go/quic-go` (pinned to v0.42.0) for HTTP/3. The module stays on
  Go 1.21; newer quic-go releases would push the floor to 1.26.
- Terminal output now writes explicit `\r\n`. Reading keys requires raw mode, which turns
  off the terminal's newline translation; without this the status bar would staircase.
- Adds `golang.org/x/term` (pinned to a Go 1.21 compatible release) for portable raw mode.
- **`-c` is now optional and defaults to auto.** Leaving it out (or passing `-c 0`) enables
  the auto ramp described above; a positive value keeps the old fixed concurrency.
- **The default address family is now IPv4.** A benchmark result must not depend on which
  family the resolver happened to prefer; pass `-6` when you actually want to exercise
  IPv6.
- `Ctrl+C` now cancels in-flight requests and restores the terminal.
- Invalid `-H` values and invalid `-i` values (for example an address that includes a
  port) are reported at startup instead of being silently ignored.
- Target URLs must start with `http://` or `https://`.
- `go.mod` now declares Go 1.21, so the global `math/rand` source is seeded
  automatically.
- Build outputs are ignored by git.
