# webBenchmark
http benchmark tool to ran out your server bandwidth.

用户在使用本工具前请先查看授权及免责声明，webBenchmark仅仅是一个用于测试网页服务器性能的工具，用作其他用途，后果自负。

Current version: 0.10 — see [CHANGELOG.md](CHANGELOG.md) for what changed.

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

# Todo 
- automatically tune concurrent routines to gain maximum performance. 

# Usage
    webBenchmark -s [URL]                   auto ramp (no -c)
    webBenchmark -c [COUNT] -s [URL] -r [REFERER]
    -c int
          concurrent routines; omit or 0 to ramp up automatically until throughput stops improving
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
    -fc string
          country code used to pick X-Forwarded-For addresses (default "cn")
    -d
          resolve the real target from the Location header
    -sub string
          subscribe url that returns the target url
    -nr
          open a new TCP connection for every request instead of reusing connections
    -t int
          per-request timeout in seconds, 0 disables it (default 30)
    -4
          force IPv4 only; this is the default
    -6
          force IPv6 only instead of the default IPv4
    -dns string
          dns server used for the target ip display (default "8.8.8.8")

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

## Behaviour notes

- **Auto ramp (default).** Without `-c` the tool starts at 4 connections, measures a
  4 second window, and doubles whenever throughput beats the best result so far by more
  than 10%. It stops after two consecutive windows without such a gain and then holds the
  concurrency that actually produced the **best** result — not the last one it tried, so a
  probe that overshoots does not leave you running at a worse setting. A window containing
  failures is never treated as an improvement. The progress is written to stderr once and
  stays visible in the status bar:

  ```
  Mode:auto  Phase:settled  Conc:8  Peak:552.000KB/s@8
  Requests:2  Failed:0  Loaded:14.277MB  Avg:503.839KB/s  Elapsed:29s
  Ramp:
    conc=4     472.000KB/s  118.000KB/s/conn       -
    conc=8     480.000KB/s   60.000KB/s/conn   +1.7%
    conc=8     552.000KB/s   69.000KB/s/conn  +15.0%
    conc=16    440.000KB/s   27.500KB/s/conn  -20.3%
    conc=16    523.412KB/s   32.713KB/s/conn   -5.2%  <- plateau
  ```

  The `s/conn` column is the diagnostic for "more concurrency but no more
  throughput": when it divides roughly in half every time the concurrency doubles while
  the total stays flat, the server is capping you per client address and no amount of
  parallel connections will help. Measured example: one host served ~450 KB/s to a single
  address no matter whether that address used 1 or 8 connections, and two different
  addresses running at the same time each still got ~450 KB/s.

  Comparisons are made against the historical peak rather than the previous window,
  because a single 4 second window on a long haul link can swing by 30%. Pass `-c N` for
  a fixed level when you already know what you want.

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
- **Status bar.** `Requests` / `Failed` / `Loaded` count completed responses, failed
  requests and downloaded bytes, `Avg` is the average throughput of the whole run and
  `Elapsed` its duration. A `LastError` line appears as soon as something fails, and each
  distinct error is printed once on stderr, so a run that produces no traffic tells you
  why instead of just showing `Failed` climbing. A request only counts as completed once
  its body has been read to the end, so a body cut short by `-t` shows up in `Failed`
  instead of silently inflating `Requests`. If `Failed` climbs while `Loaded` stays low,
  either the target is slow or `-t` is too short for the file size you are pulling — raise
  `-t` (or set `-t 0`) before trusting the throughput number.
- **Exit.** `Ctrl+C` cancels in-flight requests and restores the terminal.

## Build from source

Requires Go 1.21 or newer.

    go build -o webBenchmark_x64.exe .
    go test ./...

Cross compile all targets at once with `build.bat` (windows shell), which produces the
four binaries listed above.

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

