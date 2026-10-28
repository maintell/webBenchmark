# Changelog

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

- `-nr` — open a new TCP connection per request.
- `-fc` — country code for the `X-Forwarded-For` address pool (default `cn`).
- `-dns` — DNS server used for the target address display (default `8.8.8.8`).
- Status bar fields `Requests`, `Failed` and `Loaded`.
- Regression tests covering every item above; `go test -race ./...` is clean.
- `-h` output now documents the flags with working examples.

### Changed

- `Ctrl+C` now cancels in-flight requests and restores the terminal.
- Invalid `-H` values and invalid `-i` values (for example an address that includes a
  port) are reported at startup instead of being silently ignored.
- Target URLs must start with `http://` or `https://`.
- `go.mod` now declares Go 1.21, so the global `math/rand` source is seeded
  automatically.
- Build outputs are ignored by git.
