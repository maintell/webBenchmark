# webBenchmark
http benchmark tool to ran out your server bandwidth.

用户在使用本工具前请先查看授权及免责声明，webBenchmark仅仅是一个用于测试网页服务器性能的工具，用作其他用途，后果自负。

Current version: 0.14 — see [CHANGELOG.md](CHANGELOG.md) for what changed.

- random User-Agent on every Request
- customizable Referer Url,
- customizable header,
- concurrent routines as you wish, depends on you server performance.
- **omitting `-c` ramps concurrency up automatically** until throughput stops improving,
  then settles on the measured best value instead of a fixed guess.
- http post mode, with optional random payload size
- specify multi target ip, or resolved by system dns.
- randomly X-Forwarded-For and X-Real-IP (default on), picking addresses from a per-country CIDR list with a local fallback when the list is unreachable.
- reuses connections by default; use `-nr` to force a new TCP connection per request.
- `-4` / `-6` pin the address family. **IPv4 is the default**: IPv6 egress is frequently
  fine to connect and slow to transfer, and Go never falls back on slowness, so the
  default has to be the predictable one. Use `-6` only when you mean to test IPv6.
- live status shows request count, failure count, downloaded bytes and average throughput.
- **distributed mode**: one master with a web console drives many agents over a single
  end-to-end encrypted WebSocket, with one-time join tokens, a per-install CA, plain
  http or https, and optional SSH auto-install. See [Distributed mode](#distributed-mode).

# Todo 
- automatically tune concurrent routines to gain maximum performance. 

# Usage
    webBenchmark -s [URL]                   auto ramp (no -c)
    webBenchmark -c [COUNT] -s [URL] -r [REFERER]
    -c int
          concurrent routines; omit or 0 to ramp up automatically until throughput stops improving
    -ramp-step float
          auto mode: multiplicative concurrency step while climbing (default 1.5, i.e.
          4 -> 6 -> 9 -> 14 -> 21 ...). Doubling used to skip straight past the real peak
          and land in the region where more connections are slower
    -ramp-gain float
          auto mode: how much a window must beat the historical peak to count as an
          improvement while climbing (default 0.1, raised automatically when the link is
          too noisy to tell a real gain from a swing)
    -ramp-refine-gain float
          auto mode: the smaller gain that counts as an improvement during the refine
          pass (default 0.03, must be below -ramp-gain)
    -ramp-window int
          auto mode: length of one measurement window in seconds (default 4)
    -ramp-duel
          auto mode: before settling, measure the chosen level and one step above with
          the same number of windows and keep the faster one (default true)
    -s string
          target url, must start with http:// or https://
    -r string
          referer url
    -i string
          custom ip address for that domain, multiple addresses are picked at random;
          the port always comes from -s, so non standard ports work
    -H value
          custom header, e.g. -H 'regid:123' -H 'sign:Random16';
          a Random<N> key or value expands to N random letters
    -p string
          post content
    -rp
          send random post content on every request
    -rps int
          size in bytes of the random post content (default 1024)
    -f
          randomize X-Forwarded-For and X-Real-IP (default true)
    -px
          spoof the whole set of proxy headers with a random client address on every
          request; use it to check whether your own rate limits can be bypassed by a
          forged address
    -proto string
          protocol to benchmark with: http1, http2 or http3 (default "http1")
    -noprobe
          skip the startup protocol probe (it costs one extra request)
    -fc string
          country code used to pick X-Forwarded-For addresses (default "cn")
    -d
          resolve the real target from the Location header
    -sub string
          subscribe url that returns the target url
    -nr
          open a new TCP connection for every request instead of reusing connections
    -t int
          idle timeout in seconds: a request fails only when no data arrives for this
          long, 0 disables it (default 30). A slow but steady download never times out,
          however long the whole file takes
    -4
          force IPv4 only; this is the default
    -6
          force IPv6 only instead of the default IPv4
    -dns string
          dns server used for the target ip display (default "8.8.8.8")
    -raw
          measure raw socket throughput: after the TLS handshake the encrypted stream
          is discarded without decrypting. No status codes, no per-request accounting;
          tcp + http1 only (default false)
    -rcvbuf int
          cap the kernel receive buffer per connection in bytes (default 0 = let the
          kernel auto-tune). Only for machines where RAM, not bandwidth, is the
          constraint: setting it disables BDP auto-tuning, so a high-RTT link gets a
          lower per-connection ceiling. TCP only, ignored by -proto http3

# Download

    https://github.com/maintell/webBenchmark/releases/latest

Asset names:

    webBenchmark_linux_x64      linux / amd64
    webBenchmark_linux_arm      linux / arm
    webBenchmark_x64.exe        windows / amd64
    webBenchmark_bsd_x64.exe    freebsd / amd64

# Usage on Linux

    wget https://github.com/maintell/webBenchmark/releases/latest/download/webBenchmark_linux_x64
    chmod +x webBenchmark_linux_x64
    ./webBenchmark_linux_x64 -c 32 -s https://target.url

## Advanced example
    # send request to 10.0.0.1 and 10.0.0.2 for https://target.url with 32 concurrent threads 
    # and refer is https://refer.url 
    ./webBenchmark_linux_x64 -c 32 -s https://target.url -r https://refer.url -i 10.0.0.1 -i 10.0.0.2
    # send request to https://target.url with header regid:123 and sign:Random10
    ./webBenchmark_linux_x64 -s https://target.url -H 'regid:123' -H 'sign:Random10'
    # same, but open a fresh TCP connection for every request instead of reusing it
    ./webBenchmark_linux_x64 -s https://target.url -nr
    # measure the socket rather than the protocol: discard the encrypted stream without
    # decrypting it. Roughly halves the client CPU; no status codes, no request counter
    ./webBenchmark_linux_x64 -raw -c 48 -s https://target.url
    # low-RAM client: cap the kernel receive buffer per connection (1MB total instead of 25MB)
    ./webBenchmark_linux_x64 -c 8 -rcvbuf 262144 -s https://target.url
    # noisy link: longer measurement windows make the automatic ramp less jittery
    ./webBenchmark_linux_x64 -s https://target.url -ramp-window 8

# Distributed mode

One host runs out of its own NIC long before the target does. The same binary can act
as a **master** (web console plus control plane) or as an **agent** (load generator).
Agents only dial outwards; the master is the only process listening on a port.

The master speaks plain **http** or **https**, your choice. The agent channel is a single
encrypted WebSocket either way — see [Security model](#security-model) for why that is
not the same as "https only".

## Quick start

    # 1. master: console and control plane on one port
    ./webBenchmark -master 0.0.0.0:8443             # https, self signed (default)
    ./webBenchmark -master http://0.0.0.0:8080      # plain http, e.g. behind a tls proxy

    # 2a. on a load generator, paste the join command printed by the master
    ./webBenchmark -join wb1_eyJ2Ijo...

    # 2b. or let the master install them over ssh: console -> "SSH 安装"

    # 3. open the console, tick the nodes, fill in the target, run

At startup the master prints the console URL, the CA fingerprint and a one-time join
command. The console password is generated on first start and shown **once**; keep it,
or pin your own with `-admin-password`.

## How a task runs

    browser ──http(s)──▶ master ◀──encrypted WebSocket──▶ agent(s) ──http(s)──▶ target

- The master splits the requested total concurrency across the selected nodes
  (`ceil(total / nodes)`, clamped to 1..256 per node) and pushes every node the *same*
  task spec plus its own shard size.
- Commands, heartbeats, progress and results all travel over that one WebSocket: the
  master pushes `start` / `stop`, the agent pushes a sample every second (cumulative
  requests, failures, bytes, last error).
- The master derives rates from successive counters rather than from agent clocks, so
  clock skew between machines cannot invent a throughput number. Totals are the sum of
  the shards; peak RPS is never reported below the average.
- A task ends at its duration, on the console's stop button, or when the master marks a
  node as lost (no progress for 25 s past the deadline). The final state says which:
  `done`, `stopped` or `degraded` (some nodes never reported).

## Security model

| Concern | Control |
| --- | --- |
| Channel encryption | Every agent session runs an application-layer handshake inside the WebSocket: X25519 ephemeral keys, HKDF-SHA256 and AES-256-GCM with a strictly increasing frame counter. It is end-to-end with respect to the master process, so a TLS-terminating proxy in front of the console cannot weaken it, and plain `http` mode still gets an encrypted agent channel. Replaying or reordering a frame breaks the counter and drops the session. |
| Master identity | The master generates its own ECDSA P-256 CA on first start and signs the handshake transcript with it. The agent only needs that CA's SHA-256 fingerprint (carried in the join credential) to know it is talking to the right master. Nothing is trusted from the system root store. |
| Agent identity | Each agent holds its own client certificate (90 days) signed by that CA and signs the same transcript with its private key. The master maps the certificate CN to a node record and re-checks the certificate serial on every handshake. |
| Joining | One-time token: HMAC-SHA256 signed, expiring (default 30 min), single use (nonce recorded). The CSR and the issued certificate travel inside the encrypted channel, so even `http` mode does not expose them. |
| First contact | The agent compares the CA certificate presented during the handshake against the pinned fingerprint *before* deriving keys. A man in the middle cannot impersonate the master, and replaying an old handshake fails because both sides contribute a fresh nonce. |
| Revocation | Revoking a node closes its live connection immediately and makes the next handshake fail. Certificates are serial-checked, so a superseded certificate stops working the moment it is renewed. |
| TLS (optional outer layer) | With `https`, the console and the API are additionally wrapped in TLS 1.3 using the master's own certificate. The agent accepts either that certificate or a public one from a proxy, and never relies on TLS for identity. |
| Console auth | bcrypt password hash (`admin.hash`), HttpOnly + Secure + SameSite=Strict session cookie, a CSRF header on every state-changing call, and per-IP login throttling with exponential lockout after 5 failures. |
| SSH install | Host keys are trust-on-first-use and recorded in `<data>/ssh_known_hosts`; a changed key is refused. SSH credentials are used once and never written to disk. Only the master's own executable is uploaded — nothing is downloaded and executed on the target. |
| Secrets at rest | `ca.key`, `admin.hash`, `secret.key` and agent keys are written 0600 inside the data directory. The console password is never stored in plaintext. |
| Audit | `audit.log` (JSONL) records enrolment, rejected enrolments, token issuance, revocations, logins, SSH installs and every task start/stop/result. |
| Blast radius | Agents listen on no port at all, so a compromised load generator cannot be used as a back door into the fleet. A task is limited to 64 nodes, 256 connections per node and 24 hours. |

Two things to know before deploying:

- **Plain `http` protects the agent channel but not the browser.** The login password and
  the session cookie are only as safe as the network path. Use `http` behind a TLS proxy
  or on a trusted network; the master prints a warning at startup when you do.
- **The console certificate is self-signed under `https`**, so a browser warns once.
  Accept it or import `<data>/ca.crt` — the fingerprint printed at startup is what you
  compare against. When you reach the console through an address the master could not
  predict, add it with `-master-san 10.1.2.3`.

## Installing agents over SSH

The console's **SSH 安装** dialog logs into each host, uploads the master's own binary to
`~/.webBenchmark-agent`, starts it in the background with a fresh one-time token, and
waits until it connects back.

- Needs a POSIX shell (`uname` must work) and either a password or a private key.
- The first connection shows the host key fingerprint and installs nothing until you
  confirm it; afterwards a changed host key is refused.
- The binary must match the target platform; this is checked with `uname -sm` before the
  upload. For a mixed fleet, point `-agent-binary` at the matching build.
- Logs land in `~/.webBenchmark-agent/agent.log` on the target.

## Operations

| Path | Meaning |
| --- | --- |
| `<data>/ca.crt`, `ca.key` | Cluster trust root. Keep `ca.key` private; deleting it invalidates every enrolled node. |
| `<data>/secret.key` | HMAC key for join tokens and sessions. Rotating it invalidates pending tokens and logins. |
| `<data>/state.json` | Node registry and already used join-token nonces. |
| `<data>/tasks/<id>.json` | Final result of every task, kept for the console history. |
| `<data>/ssh_known_hosts` | Trusted SSH host keys (trust on first use). |
| `<data>/audit.log` | Append-only audit trail. |

- Data directories default to `./webBenchmark-master` and `./webBenchmark-agent`; use
  `-data` to relocate them.
- An enrolled agent keeps its identity in its data directory: restarting it needs no new
  token. Pointing an enrolled directory at a different master is refused instead of
  silently re-enrolling.
- Agents renew their own certificate in the last 30 days of validity; the old
  certificate stops working the moment the new one is issued.
- `-join-ttl` changes the token lifetime, `-node-name` sets the name shown in the console.
- `-admin-password` is handy for automation but ends up in the process list; prefer the
  generated password shown once at first start, and use the flag only to reset it.

## Distributed mode options

    -master string
          run as the master node (web console and agent control plane), e.g. -master 0.0.0.0:8443 or -master http://0.0.0.0:8080
    -agent
          run as an agent node and take tasks from the master
    -join string
          join credential printed by the master; implies -agent
    -master-url string
          master base url for agent mode, e.g. https://10.0.0.1:8443
    -join-token string
          one-time join token, alternative to -join
    -ca-fingerprint string
          master CA sha256 fingerprint, alternative to -join
    -ca-cert string
          path to the master ca.crt, used instead of -ca-fingerprint
    -agent-binary string
          path to the agent binary used by the ssh installer; defaults to the running executable
    -data string
          state directory; defaults to ./webBenchmark-master or ./webBenchmark-agent
    -node-name string
          agent name shown in the console (default: hostname)
    -admin-password string
          master console password, at least 10 characters; a random one is generated when omitted
    -master-san string
          extra comma separated SANs for the master certificate (ip or dns name)
    -join-ttl int
          join token lifetime in minutes (default 30)

## HTTP API

The console is a thin client over the same API, so everything can be scripted. Admin
calls need the session cookie from `POST /api/v1/login` plus the `X-WB-CSRF` header
returned by it; the agent channel authenticates inside the encrypted WebSocket handshake.

    WS   /api/v1/agent/ws           encrypted control channel: hello/challenge/auth,
                                    then start, stop, progress, result, heartbeat, renew
    POST /api/v1/login              console password -> session cookie + CSRF token
    GET  /api/v1/session            installation info, auth state, csrf token
    GET  /api/v1/nodes              node list with online state and live rates
    POST /api/v1/nodes/{id}/revoke  block a node permanently and drop its connection
    GET  /api/v1/tasks              recent tasks (running first, then history)
    POST /api/v1/tasks              create and dispatch a task
    GET  /api/v1/tasks/{id}         live snapshot: totals, per-node rows, series
    POST /api/v1/tasks/{id}/stop    stop a running task
    POST /api/v1/join               issue a fresh join command for a given master url
    POST /api/v1/ssh/probe          connect over ssh and return the host key fingerprint
    POST /api/v1/ssh/install        upload the agent binary, start it, wait until it connects

## Behaviour notes

- **Which protocol is actually in use?** `-proto` picks `http1` (default), `http2` or
  `http3`, and the startup probe prints what the server really offers:

  ```
  probe: alpn=h2 alt-svc=none -> server supports http2 only (-proto http2)
  probe: alpn=h2 alt-svc=h3=":443"; ma=86400 -> server advertises http3 (-proto http3)
  ```

  The verdict comes from two independent signals. ALPN tells you whether HTTP/2 can be
  negotiated. HTTP/3 is **not** negotiated by ALPN — the only standard way to discover it
  is an `Alt-Svc` header carrying `h3`, so a missing `Alt-Svc` means "this server does not
  offer h3", not "the probe failed". Two real examples: a bare nginx answered `alpn=h2`
  with no `Alt-Svc`, so h3 is unavailable there; `www.cloudflare.com` answered `alpn=h2`
  plus `alt-svc: h3=":443"`, and `-proto http3` then completed 221 requests over QUIC.

  HTTP/2 has to be asked for explicitly. Go disables it whenever a custom `TLSClientConfig`
  or `DialContext` is supplied, which this tool needs for `-i` and the address family
  filter, so `-proto http2` sets `ForceAttemptHTTP2`. HTTP/3 goes over QUIC, which the Go
  standard library does not implement at all; it is provided by `quic-go`, and `-i` / `-4`
  / `-6` still apply through a QUIC dial hook.

  Note that multiplexing is not automatically faster: against a target that caps each
  client address, one h2 connection is just as BDP limited as one h1 connection. Measured
  on the same host: 508 KB/s with eight http1 connections versus 460 KB/s with http2.

- **Auto ramp (default).** Without `-c` the tool finds the concurrency that actually
  produces the most throughput, in two passes.

  *Climb.* The first window is thrown away as warm-up: it carries the handshakes, DNS and
  TCP slow start, and measured about 40% of the steady state on the test box, which would
  bias the starting level downwards. After that, a window (`-ramp-window`, default 4 s)
  that beats the historical peak by more than `-ramp-gain` (default 10%) multiplies the
  concurrency by `-ramp-step` (default 1.5). Two consecutive windows without such a gain
  end the climb — but not before one **breakout probe** one step higher. A plateau caused
  by one unlucky level is otherwise indistinguishable from a real one, and that is exactly
  how the ramp once settled on 6 connections at 48.8 MB/s while 8 connections did
  62.6 MB/s on the same box. The breakout probe is judged without the dead zone: after two
  windows of a confirmed plateau, merely beating the previous peak means the plateau was
  false and the climb resumes. A window containing failures never counts as an improvement.

  The peak, and therefore the level the ramp settles on, follows the highest rate actually
  observed even when the gain over the previous peak is inside the dead zone. The dead zone
  decides whether to keep climbing; it must not throw away a better measurement.

  *Refine.* The climb leaves a bracket: the level that measured best (`low`) and the first
  level confirmed worse (`high`). The refine pass probes geometric midpoints inside that
  bracket, two windows per candidate averaged, and accepts a candidate only when it beats
  the incumbent by `-ramp-refine-gain` (default 3%). It keeps three points (`low < best <
  high`) and always probes whichever side of the bracket is wider, because probing only
  upwards has a blind spot: with the real peak at 37 and 32/48 already measured, the first
  probe lands on 39, which beats 32, and a one-sided search then never looks back down and
  reports 39. It settles once both sides are within `max(2, 5%)` of the incumbent, and on
  a tie it keeps the **smaller** concurrency — same throughput, fewer connections and less
  CPU.

  The settled level is the one that measured best, never the last one tried, so a probe
  that overshoots does not leave you running at a worse setting.

  *Duel.* Before the ramp declares itself settled it re-measures the chosen level and one
  step above it, the same number of windows each, and keeps the faster one. The peak may
  have come from a single window, and a single window on a long haul link swings by ±20%:
  one run had 9 connections at 61.4 MB/s from one window while 14 connections measured
  68.1 and 72.1 in two, and the ramp stopped at 9. The duel is what separates "a lucky
  window" from "actually faster". It costs four windows; `-ramp-duel=false` turns it off.

  Both passes run against
  the historical peak rather than the previous window, because a single window on a long
  haul link swings by 30%; when two windows measured at the *same* level disagree by more
  than the dead zone, that spread is used as the gate instead of the fixed 10%, so a noisy
  link stops treating noise as a trend. A link that produces no bytes at all settles at the
  starting level instead of ramping to the cap.

  Progress is written to the status bar. Labels form a fixed column and the two variable
  blocks (ramp, interfaces) are tables, so numbers line up and can be scanned vertically:

  ```
  Target  https://example.com/big.iso
  IP      203.0.113.7,203.0.113.8
  Mode    auto refine   conc 19   bracket 16..24
  Rate    avg 41.905MB/s   now 43.120MB/s
  Volume  1.204GB   3 req   0 failed   31s
  CPU     23.3% proc / 89.9% busy   softirq 21.4%   load 6.63 2.80 2.70
  Mem     11.1MB rss / 80.3% sys
  Ramp    conc        rate   per-conn   vs peak
             6  28.400MB/s  4.733MB/s        -
             9  51.020MB/s  5.669MB/s   +79.6%
            14  68.320MB/s  4.880MB/s   +33.9%
            21  70.110MB/s  3.339MB/s    +2.6%
            21  66.240MB/s  3.154MB/s    -5.5%   plateau
  Net     iface            down/s       up/s   total down/up
          eth0         49.124MB/s 789.361KB/s   118.115GB/48.403GB
          lo           303.000B/s 303.000B/s   30.616GB/30.616GB
          (others)                             (6 quiet)
  ```

  The interface table is sorted by current traffic, capped at the four busiest, and folds
  every idle `docker`/`veth`/`lo` into the `(others)` count — a host with a dozen virtual
  interfaces used to print a dozen lines every second and push everything else off screen.
  Those counters belong to the **host**, not to the run: on a box that also runs proxies or
  containers, `eth0` reads higher than `Rate now`, and the totals include traffic this
  benchmark never sent.

  The `per-conn` column is the diagnostic for "more concurrency but no more
  throughput": when it keeps falling while the total stays flat, the server is capping you
  per client address and no amount of parallel connections will help. Measured example: one
  host served ~450 KB/s to a single address no matter whether that address used 1 or 8
  connections, and two different addresses running at the same time each still got
  ~450 KB/s.

  Pass `-c N` for a fixed level when you already know what you want.

- **Live concurrency keys (auto mode, interactive terminal only).** While the run is in
  progress you can steer the concurrency without restarting:

  | key | effect | key | effect |
  | --- | --- | --- | --- |
  | `1` | +10% | `5` | −10% |
  | `2` | +20% | `6` | −20% |
  | `3` | +30% | `7` | −30% |
  | `4` | ×2 | `8` | ÷2 |

  The first key press takes control: automatic ramping stops, the phase switches to
  `manual`, and the status bar shows what you pressed (`Conc:14  (user -30%)`). Further
  presses keep working. `Ctrl+C` still exits. Keys need a real terminal — when stdin is
  redirected the `Keys:` line is omitted and everything else behaves the same.

- **Connection reuse.** By default every routine keeps one persistent connection and
  reuses it, which measures application level throughput. `-nr` disables keep-alive so
  each request pays a full TCP (and TLS) handshake; use it to measure handshake overhead
  or to emulate clients that never reuse connections.
- **`-i` and ports.** `-i` only replaces the address that gets dialed. The port always
  comes from `-s`, so `-s https://host:8443` dials `<ip>:8443`, and HTTPS targets keep
  working because the dial hook no longer forces port 80.
- **`-d` looks at the first hop only.** It sends one request and reads the `Location`
  header without following the redirect. The probe therefore never adds traffic to the
  real target, and it still works when the redirect destination is unreachable. Relative
  `Location` values are resolved against `-s`. The target is re-checked every 60 seconds.
- **`-f` / `-fc`.** Addresses are picked from a per-country CIDR list that is downloaded
  once in the background at startup. The list is never fetched on the request path, so a
  slow or blocked CDN cannot stall the benchmark; until it arrives (or if it never does)
  a fully random address is used. A failed download is reported once on stderr.

- **`-px`, and what it can actually prove.** It fills every common proxy header
  (`X-Forwarded-For`, `X-Real-IP`, `X-Client-IP`, `True-Client-IP`, `CF-Connecting-IP`,
  `Fastly-Client-IP`, `X-Cluster-Client-IP`, `X-Originating-IP`, `X-Remote-IP`,
  `X-Remote-Addr`, `X-Proxy-User-Ip`, `X-Original-For`, the RFC 7239 `Forwarded` header,
  plus `X-Forwarded-Proto` / `X-Forwarded-Host` matching the real request) with a fresh
  forged address on every request.

  It only changes anything when the target **trusts** one of those headers, which is
  normal behind a CDN or reverse proxy but never the case for a server that is reached
  directly. There the limit is applied to the TCP source address and no header can move
  it. Measured on a directly reachable host that caps clients per address: 501.9 KB/s
  without `-px`, 502.5 KB/s with it, and 439.9 KB/s at four times the concurrency — the
  forged headers changed nothing, which is itself the answer to the test.

  Use it against infrastructure you are responsible for, together with the disclaimer in
  this repository.
- **`-4` / `-6`, and why IPv4 is the default.** A dual stack host can have one family
  connecting fine while transferring slowly, and neither Go nor curl will fall back just
  because a working connection is slow. Measured on one VPS against the same Cloudflare
  endpoint, IPv6 delivered 70 KB/s and IPv4 delivered 143 MB/s. A benchmark whose answer
  depends on which address family the resolver happened to prefer is not a benchmark, so
  IPv4 is the default and `-6` is the explicit opt in. If the target has no IPv4 address
  the tool stops with an error telling you to add `-6`, rather than silently switching.
- **Status bar.** `Rate` is the average throughput of the run next to the current
  one-second rate, `Volume` is downloaded bytes plus completed / failed requests and the
  elapsed time. `Failed` is broken down into `timeout:` (nothing arrived for
  `-t` seconds) and `rejected:` (the target answered 4xx/5xx, refused the connection or
  broke it mid-body), so an idle timeout is never mistaken for the target pushing back.
  Status codes above 399 used to be counted as *successful* requests, which could report a
  green run against a target that answered nothing but `403`. A `LastError` line appears
  as soon as something fails, and each distinct error is printed once on stderr, so a run
  that produces no traffic tells you why instead of just showing `Failed` climbing. A
  request only counts as completed once its body has been read to the end, so a body cut
  short shows up in `Failed` instead of silently inflating `Requests`.
- **`CPU:` has two numbers, and the difference matters.** `CPU 45.3% proc / 89.9% busy   softirq 21.4%`
  means this process used 17.3% of one core while the machine as a whole was saturated. The
  single number this line used to print was the machine-wide figure, which reads like "the
  load generator is the bottleneck" when it usually is not: measured on a 1 vCPU box at
  45 MB/s, the process itself used 31% of the core and the rest was other daemons plus
  network softirq. `Memory:` likewise shows this process's RSS next to machine-wide usage.
  Diagnose from the first number; the second one tells you what the box is doing.
- **Small machines.** Two costs scale with concurrency and neither shows up in the
  process's own RSS: the kernel receive buffer per connection (auto-tuned to the
  bandwidth-delay product — about 2.5 MB on a 0.8 s RTT link) and the CPU of the network
  stack. Sixteen connections measured 44 MB of kernel socket memory, which on a 1 GB box
  with a 150 MB free margin is enough to push everything else into swap; the swap traffic
  then becomes disk I/O and throughput collapses for reasons that have nothing to do with
  the target. On such a host, watch the `swap ... used` figure on the `Mem` line, cap the
  buffer with `-rcvbuf`, and note that the ramp stops itself when free memory falls below
  `max(5%, 64 MB)` rather than piling on more connections.
- **`-raw` measures the socket, not the protocol.** It performs the TLS handshake, writes
  the request, and then discards the *encrypted* stream without decrypting it — on Linux
  through a pipe to `/dev/null`, so the bytes never enter user space. That removes AES-GCM,
  the copy to user space and HTTP parsing: exactly the part of the cost the load generator
  controls. Measured on a 1 vCPU host at 48 connections, the process went from 33–49% of a
  core in normal mode to **17–20%** in `-raw` — about half the CPU for the same bytes.

  Throughput did not move, and that is the takeaway: `softirq` was already at 40–50% of the
  core, so the receive path, not the generator, was the ceiling. Read the `softirq` figure
  before optimising anything — it is the half of the picture no user-space tool can touch,
  and it decides whether a faster client would help at all.

  What you give up: status codes (a `403` hotlink rejection looks like a very short
  transfer), the request counter (the status bar shows `N sent` instead of `N req`), the
  content itself, and any knowledge of where one response ends. Re-requests are driven by
  a byte budget derived from `Content-Length`, with an idle timeout as the fallback, so an
  endpoint that answers `403` to everything will simply look idle. `Failed` therefore only
  counts connection-level errors. The auto ramp still works — it only needs bytes and
  failures.

  Use it when the generator is the bottleneck and you want to know what the link can really
  do; do not use it to decide whether a target is healthy.

- **Exit.** `Ctrl+C` cancels in-flight requests and restores the terminal.

## Build from source

Requires Go 1.21 or newer.

    go build -o webBenchmark_x64.exe .
    go test ./...

Cross compile all targets at once with `build.bat` (windows shell), which produces the
four binaries listed above.

## Release

**All quality gates run locally; the pipeline only compiles and publishes.** Nothing is
built on a branch push or a pull request — a release starts only from a version tag, and a
single job produces all four binaries.

    pwsh ./scripts/preflight.ps1 -Tag 0.10   # gofmt, vet, test, race, four targets, docs
    git tag 0.10 && git push origin 0.10      # this is the release

The preflight script refuses to pass when the working tree is dirty, when any file needs
gofmt, when vet or the tests fail, when any target does not compile, when `CHANGELOG.md`
has no `## <version>` section, when the README version line is stale, or when a flag
exists in the code but is missing from the flag table in this file.

The workflow then re-checks three things before it publishes, because a mismatched
release is worse than no release: the tag must exist, it must equal `const version` in
`main.go`, and it must have a matching changelog section. It attaches the four binaries
plus `SHA256SUMS` and uses the changelog section as the release notes. Re-running it for
an existing tag refreshes the assets instead of failing.

## LICENSE AND DISCLAIMER


**1. Application.**

Please read this document carefully before using, accessing, downloading, installing or otherwise operating the webBenchmark as defined hereafter.

Using, accessing, downloading or otherwise operating any of the webBenchmark, constitutes an unconditional agreement by You to be bound by this the following terms and conditions for the time of Using the webBenchmark and thereafter.

IF YOU DO NOT ACCEPT THE TERMS OF THIS LICENSE AGREEMENT, YOU ARE PROHIBITED FROM USING ANY OF THE webBenchmark.

**2. Definitions.**

**"webBenchmark"** shall mean any of the documents, description, explanations, presentations, media types, all schedules, appendixes and related documentation, software in object or source code, including Updates provided on this Platform by Licensor for Your Use.

**"Derivative Works"** means any modification, change, adaptations, contributions, enhancements, customization, modifications, inventions, developments, improvements of the Date Product by you and not developed by Licensor or integrated into the Date Product by Licensor.

**"Intellectual Property Rights"** means any intellectual property and proprietary rights, including , but not limited to, copyrights, moral rights, works of authorship, trade and service marks, trade names, rights in logos and get-up, inventions and discoveries, and Know-How, registered designs, design rights, patents, utility models, all rights of whatsoever nature in computer software and data, source code, database rights all intangible rights and privileges of nature similar or allied to any of the foregoing, in every case in any part of the world and whether or not registered; and including all granted registrations and all applications for registration, all renewals, reversions or extensions, the right to sue for damages for past infringement and all forms of protection of a similar nature which may subsist anywhere in the world.

**"Know-How"** means any information relating to commercial, scientific and technical matters, inventions and trade secrets, including but not limited to any patentable technical or other information which is not in the public domain including information comprising or relating to concepts, discoveries, data, designs, formulae, ideas, reports and data analyses.

**"License"** shall mean this license and disclaimer document and its terms and conditions for use, reproduction, and distribution as provided in this document.

**"Licensor"** shall mean the copyright owner or entity authorized by the copyright owner that is granting the License, meaning maintell, and its successors and assigns.

**"Parties"** means both You and Licensor.

**"Party"** means You or Licensor individually.

**"Platform"** means the maintell GitHub account and related repositories available at https://github.com/maintell.

**"Purpose"** means using or integrating the webBenchmark free of charge for the purpose of using and integrating benchmarking on a website, whereby examples are provided in the webBenchmark to demonstrate specific features.

**"SDK"** means a software development kit which is a set of software development tools that allows the creation of applications for a certain software package, video service platforms, software framework, or similar development platform.

**"maintell"** means a set of tools written and developed by the Licensor that provides support for benchmark and related functionalities for HTTP including any related software, source and object code, deliverables, technology and related resources and relevant documentation provided and/or created, made available, license and/or sold to you and developed by Licensor in connection with separate license terms and conditions.

**"Use"** means using, accessing, downloading, installing or otherwise operating or using the webBenchmark as part of Your self-service and subject to clause titled "LICENSE" and in connection with the Purpose of this License and its terms and conditions.

**"Updates"** means all updates, modifications and releases of new versions of webBenchmark containing improvements, corrections, minor modifications, bug fixes, patches, or the like that have been added to the Platform by the Licensor.

"**You" (or "Your")** shall mean an individual or legal entity exercising permissions granted by this License.

**3. License**

Subject to the terms and conditions of this License, Licensor hereby grants to You a perpetual, worldwide, non-exclusive, no-charge, royalty-free, right to reproduce, prepare Derivative Works of, sublicence, make, have made, use, import the webBenchmark and the Derivative Works as required for the Purpose and subject to the terms and conditions as described in the Date Products.

Except as otherwise agreed by Licensor in writing in separate license terms and conditions for the use of maintell, You shall not distribute, relicense, sell, lease, transfer, encumber, assign or make available for public use the webBenchmark. Any attempt to take any such actions is void and will automatically terminate Your rights under this License.

If the webBenchmark or your Use (allegedly) constitutes a direct or contributory infringement, then any

rights granted to You under this License for that webBenchmark shall terminate immediately.

Unless agreed by Parties in writing or if the enforcement of this provision is prohibited by applicable law, You shall not under any circumstances attempt, or knowingly cause or permit others to attempt to modify, adapt, port, merge, decompile, disassemble, reverse engineer, decipher, decrypt or otherwise discover the source code or any other parts of the mechanisms and algorithms used by webBenchmark nor remove restrictions or create derivative works of webBenchmark or of any part of webBenchmark.

**4. Support**

The Licensor has no obligation under this License to provide any maintenance, support or training to You.

**5. Update**

The Licensor may at any time, at its discretion provide Updates to the webBenchmark. The Licensor has, however, no obligation whatsoever under this License to provide Updates, modify or release new versions of the webBenchmark.

**6. Submission of Contributions**

Any contribution submitted for inclusion in the webBenchmark by You to the Licensor shall be under the terms and conditions of this License, without any additional terms or conditions. Such inclusion shall be subject to Licensor&#39;s discretion. Notwithstanding the above, nothing herein shall supersede or modify the terms of any separate license agreement you may have executed with Licensor regarding such contributions.

**7. Trademarks.**

This License does not grant permission to use the trade names, trademarks, service marks, or product names of the Licensor, except as required for reasonable and customary use in describing the origin of the webBenchmark and related copyright notices.

**8. Intellectual Property**

You recognize that all rights, title and interests in and to any and all worldwide Intellectual Property Rights related to the webBenchmark shall remain the property of Licensor or its suppliers. Unless otherwise agreed upon between the Parties, any Intellectual Property Rights in any Updates, contributions, enhancements, customization, modifications, inventions, developments, improvements thereof of any kind to, in, or that otherwise relate to the webBenchmark, including any Derivative Work or results of agreed services during, before or after the term of this License, either specific to You, Your customer or in general in connection with this License or arising out of the business relationship between the Parties shall solely and exclusively belong to or be transferred to Licensor through assignment, entitlement or otherwise, including the entire right, title and interest. For this purpose, Licensor shall also have the right to file and prosecute at its own expenses any patent application on the same above, in any country, region or jurisdiction in the world in its own name or on behalf of You, as the case may be. You shall not have the right to claim and will not undertake or try to obtain, register or apply for any Intellectual Property Rights or other rights in or to the webBenchmark or Derivative Works anywhere in the world. You shall not do anything that might misrepresent, change or otherwise compromises the ownership or proprietary rights of Licensor or its suppliers under this License. You shall not take any actions that would amount to an exhaustion of Licensor&#39;s or its suppliers Intellectual Property Rights. The webBenchmark may contain the logo and copyright notice of Licensor. It is prohibited to remove or modify the copyright notice and webBenchmark logo of Licensor.

**9. Disclaimer of Warranty.**

Unless required by applicable law or agreed by the Parties in writing, Licensor provides the webBenchmark AND any RELATEd SERVICE on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied, STATUTORY OR OTHERWISE, including, without limitation, any warranties or conditions of THE webBenchmark ACCURACY, TITLE, MERCHANTABILITY, or FITNESS FOR A PARTICULAR PURPOSE OR NON-INFRINGEMENT. THE LICENSOR DOES NOT PROVIDE ANY WARRANTY AS TO QUALITY, SUITABILITY, FEATURES, COMPATIBILITY OF THE webBenchmark AND RELATED SERVICES. THIS AGREEMENT DOES NOT PROVIDE ANY REPRESENTATION OR WARRANTY OR LIABILITY AS TO ANY THIRD-PARTY SOFTWARE.

**10. Third Party Software Disclaimer**

The webBenchmark may make reference to third party standard software (e.g. open source software and video test streams) which is not developed by Licensor, but which are provided in connection with the Purpose of the integration or testing of the maintell. For the avoidance of doubt, Licensor is not a sub licensor of such third party software. Licensor refers Licensee to applicable attribution files and license terms disclosures and pertinent terms of the respective third-party standard software publisher which apply directly to Licensee. However, Parties will ensure their compliance with such relevant licensing terms.

THIS THIRD-PARTY SOFTWARE IS PROVIDED BY THE RESPECTIVE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS THIRD PARTY SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

**11. Limitation of Liability.**

You are solely responsible or liable for determining the appropriateness of Using the webBenchmark AND RELATED SERVICES and assume any risks associated with Your exercise of permissions under this License and the creation of Derivative Works. Licensor shall have no liability of any kind with regards TO such Derivative Works.

In no event and under no legal theory, whether in tort (including negligence), contract, or otherwise, unless required by applicable law (such as deliberate and grossly negligent acts) or agreed to in writing, shall Licensor be liable to You for damages OF ANY KIND, including any direct, indirect, special, incidental, or consequential damages of any character arising as a result of this License or out of the use or inability to use the webBenchmark or Derivative Works, including but not limited to damages for loss of goodwill, LOST REVENUE, LOST PROFIT, LOST DATA OR CORRUPTED DATA, COSTS OF PROCUREMENT FOR SUBSTITUTION OF PRODUCTS OR SERVICES, THIRD PARTY SOFTWARE AND CLAIMS, PROVIDED INFORMATION, WASTED MANAGEMENT TIME, LOSS OF USE OF COMPUTER SYSTEMS AND RELATED EQUIPMENT, COMPUTER FAILURE AND MALFUNCTIONS, DOWNTIME COST, work stoppage, or any and all other commercial damages or losses, even if such the Licensor or the Contributor has been advised of the possibility of such damages.

THE PROVISIONS OF THIS CLAUSE TITLED "LIMITATION OF LIABILITY" SHALL NOT APPLY TO THE EXTENT RESTRICTED OR PREVENTED BY MANDATORY APPLICABLE LAW THAT CANNOT BE AMENDED OR EXCLUDED BY CONTRACTUAL WAIVER SUCH AS DELIBERATE ACTS AND FRAUD.

**12. Derivative Work**

While creating and using Derivative Works, if you choose to offer additional warranty, indemnity, or other liability obligations and/or rights inconsistent with this License, You act only on Your own behalf and on Your sole responsibility, not on behalf of the Licensor. You agree to indemnify, defend, and hold the Licensor harmless for any liability incurred by, or claims asserted against, the Licensor by reason of your accepting any such warranty or additional obligations and liability.

**13. Third Parties**

The Licensor will not indemnify nor hold harmless You against any infringements of any rights of third parties with respect to the webBenchmark or the Derivative Works.

Licensor shall have no obligation for payment of royalties or any other compensation to You or third parties, if any, with respect to the Use of the webBenchmark by You or Your customers, clients, viewers, listeners for playing media content or in connection with third party products and software. You will be exclusively responsible for payment of royalties to third parties.

**14. Legal Capacity**

By accepting this License, You represent and warrant to have the legal capacity and authority to enter into this legally binding License.

**15. No Implied Rights**

Other than expressly provided for in this License, nothing in this License grants or shall be construed to grant to any Party any right and/or any license to any Intellectual Property Right or application therefore (including but not limited to patent applications or patents) which are held by and/or in the name of the other Party and/or which are controlled by the other Party, or to any Confidential Information received from the other Party.

**16. Indemnification**

You agree, at Licensor&#39;s option, to release, defend, indemnify, and hold Licensor and its affiliates and subsidiaries, and their officers, directors, employees, contractors and agents, harmless from and against any claims, liabilities, damages, losses, and expenses, including, without limitation, reasonable legal and accounting fees, arising out of or in any way connected with (i) Your breach of this License (ii) Your negligent or improper use, misuse or intentional omission in connection with the use of the webBenchmark or any of Licensor&#39;s services.

**17. Notices**

All notices or other communication required or permitted to be given in writing under this License must be given in the English and Chinese language by email.

**18. Waivers**

No failure or delay by any Party in exercising any right or remedy provided by law or pursuant to this License will impair such right or remedy or be construed as a waiver of it and will not preclude its exercise at any subsequent time and no single or partial exercise of any such right or remedy will preclude any further exercise of it or the exercise of any other remedy.

**19. Severability**

If any provision of this License or of any of the documents contemplated in it is held to be invalid or unenforceable, then such provision will (so far as it is invalid or unenforceable) have no effect and will be deemed not to be included in this License or the relevant document, but without invalidating any of the remaining provisions of this License or that document. The Parties must then use all reasonable endeavors to replace the invalid or unenforceable provision by a valid and enforceable substitute provision the effect of which is as close as possible to the intended effect of the invalid or unenforceable provision.

**20. Modifications**

The Licensor may modify the terms of this License in its sole discretion and such modifications shall take effect and be binding on You on the earliest date which they are posted to the Platform. No one other than the Licensor has the right to modify this License.

**21. GOVERNING LAW AND JURISDICTION**

The License is governed by and must be construed, interpreted in accordance with the laws of CHINA without given effect to the conflict of law principles thereof. The courts of CHINA have exclusive jurisdiction over any dispute, legal action and proceedings arising out of or related to the License, including its termination, which shall be binding and enforceable upon the Parties worldwide. In the event of any proceeding or litigation arising out of this License, the prevailing Party shall be entitled to recover from the non-prevailing Party its legal fees, court fees and related costs to the extent and in ratio of its success. Notwithstanding the foregoing, Licensor may bring legal actions against You in the country where You has its seat, if it deems necessary for the enforceability of its rights regarding payments by You under the License.

