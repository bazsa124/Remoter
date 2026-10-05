# Tiered Remote Control — Implementation Plan

Companion to [tiered-remote-control-spec.md](tiered-remote-control-spec.md). The spec says *what*;
this says *how*, on *this* hardware, without welding the project to it.

Two constraints drive every decision below:

1. **Use what is already here.** Go, Node, Python, Rust, Docker, ffmpeg and a full Android SDK are
   installed. Nothing in Phase 0–3 requires a new toolchain.
2. **Stay OS-independent anyway.** The host happens to be Windows 11 Home on a Ryzen laptop. That is
   a *deployment target*, not an architecture. Every Windows-specific line lives behind an interface
   with a build tag, and CI cross-compiles Linux and macOS from day one so portability cannot rot
   silently.

---

## 0. Host survey (measured, 2026-09-06)

| Property | Value | Consequence for the plan |
|---|---|---|
| OS | Windows 11 **Home**, build 26200 | **No RDP host.** Phase 0's `winver` question is answered: there is no fallback. Sunshine is not "the nicer option", it is the only Tier 3. Also: no `gpedit`, no Local Users & Groups MMC — service setup goes through `sc.exe` / `New-Service` as LocalSystem. |
| Chassis | Notebook (Lenovo 82CY), battery present | Lid + Modern Standby are the real availability risk, not "sleep" in the abstract. See §6. |
| CPU | Ryzen 5 5600U, 6C/12T | Plenty for agent + encode. Encoding happens on the iGPU, not these cores. |
| GPU | Radeon (Cezanne, VCN 2.2), UMA | AMF **H.264 + HEVC** encode available; **no AV1**. Sunshine encoder: `amdvce`. Prefer HEVC on mobile data. |
| RAM / disk | 16 GB / 119 GB free on C: | Non-issue. |
| Network | **Wi-Fi only** (MediaTek MT7921), no Ethernet | Wake-on-LAN is effectively unavailable (no WoWLAN worth trusting). The strategy becomes *never sleep*, not *wake on demand*. Uplink is the Tier 3 ceiling — measure it in Phase 0. |
| Extra NIC | VirtualBox Host-Only adapter | Watch interface selection in Tailscale/Sunshine; bind explicitly, never to `0.0.0.0`. |
| Privileges | User is in Administrators, shells run **unelevated** | Every install step needs an explicit elevated prompt. One bootstrap script, run once, elevated. |
| Toolchains | Go 1.23.2 · Node 22.18 + pnpm · Python 3.13 · Rust · Docker Desktop (WSL2, stopped) · Git · ffmpeg 8.1 · Java 23 | Nothing to install for Phases 1–3. |
| Android | Android Studio + SDK (platforms 34, 36.1; build-tools 34/36.1/37; `adb` in `platform-tools`, not on PATH) | Phase 3 can start same-day. Use **Studio's bundled JBR (JDK 21)** for Gradle — system Java 23 is not an AGP-supported JDK. |
| Missing | Tailscale, OpenSSH **Server**, Sunshine, Moonlight, ntfy, any real WSL distro | Exactly the Phase 0 shopping list. |

---

## 1. Technology decisions

### Agent language: **Go**

The spec left this open ("Go or Node"). Go, for four reasons specific to this project:

- Single static binary — deployment on any OS is "copy one file", no runtime on the host.
- `GOOS=linux go build` from this Windows box: no container, no cross-compiler. Portability is
  verifiable on every commit, for free.
- `kardianos/service` abstracts Windows SCM / systemd / launchd behind one API. Service lifecycle is
  the single hardest thing to keep portable, and it is already a solved problem in Go.
- `kbinani/screenshot` covers Windows (DXGI/GDI), X11 and macOS (CoreGraphics) with one API — Tier
  1.5 is portable on day one instead of Windows-only.

Node would need a runtime installed on every future host; Rust's Windows-service story is thinner.

### Client: **SvelteKit PWA first, Kotlin second**

The PWA *is* the OS-independence story for the client half, and it skips the store loop. The Kotlin
app in Phase 3 is a shell around the same API contract, not a rewrite.

### Everything else: as the spec decided

Tailscale (network), ntfy (push), Sunshine (Tier 3 host), OpenSSH (Tier 2). All four are already
cross-platform — the portable choice and the pragmatic one coincide here. **Write no encoder, no PTY
server, no push infrastructure.**

### Deviation from the spec: skip OliveTin

The spec's Phase 0 uses OliveTin (WSL → SSH → PowerShell) to discover the real button list before
writing code. On this machine that means standing up a WSL distro and an SSH server purely to reach
a shell that is already local. A ~150-line Go stub — `actions.yaml`, `GET /actions`, `POST /run`,
plaintext log tail — costs the same evening, runs natively, keeps the AGPL boundary clear from the
start, and is not throwaway: it *is* the Phase 1 skeleton with the hard parts stubbed. The spec's
underlying instruction ("use it for two weeks before designing the real thing") is preserved
unchanged, and that is the part that matters.

Zero-code alternative if you would rather not touch Go in week one: `docker run olivetin/olivetin`
(Docker Desktop is installed) — but then it is genuinely throwaway.

---

## 2. OS-independence rules

Not aspirations — checkable constraints. Violating one should fail CI or review.

1. **No absolute paths, no drive letters, no path separators in string literals.** `filepath.Join`
   only. Config location resolves through `os.UserConfigDir()` / `%ProgramData%` / `/etc` in exactly
   one function: `internal/config.Dir()`.
2. **All OS calls behind `internal/platform`.** Interfaces defined once; implementations in
   `*_windows.go` / `*_linux.go` / `*_darwin.go`; plus a `*_stub.go` returning `ErrUnsupported` so an
   unimplemented platform still *compiles*. A red Linux build is the alarm that portability broke.
3. **The action engine never assumes a shell.** `exec.Command(cmd, args...)` with an explicit argv.
   Per-OS variants are *data* (§4), not `if runtime.GOOS ==` scattered through the engine.
4. **CI cross-compiles `windows/amd64`, `linux/amd64`, `linux/arm64`, `darwin/arm64` on every
   commit**, and runs the test suite under Linux in Docker — even while only Windows is deployed.
5. **UTF-8 in, UTF-8 out.** Windows console subprocesses default to a legacy codepage (CP852/1250
   here) and will mangle `ő ű á í` in job output. The Windows exec wrapper sets
   `[Console]::OutputEncoding` / `chcp 65001`, and the agent validates `utf8.Valid` on every captured
   line, replacing invalid runes rather than emitting broken JSON. Same trap the spec already flagged
   for keyboard input — it applies to command output too.
6. **UTC everywhere on the wire**, formatted only at render time. Job IDs are ULIDs (sortable, no
   clock-skew ties).
7. **No CRLF assumptions** in log tailing or config parsing. `.gitattributes` normalises.

---

## 3. Repository layout

```
Remoter/
├─ tiered-remote-control-spec.md
├─ IMPLEMENTATION-PLAN.md
├─ docs/adr/                     # one file per irreversible decision
├─ agent/                        # Go
│  ├─ cmd/remoter-agent/         # main + service lifecycle
│  ├─ internal/api/              # HTTP + WebSocket handlers
│  ├─ internal/actions/          # registry, validation, per-OS resolution
│  ├─ internal/jobs/             # runner, ring-buffer log, state machine
│  ├─ internal/push/             # ntfy publisher
│  ├─ internal/config/           # paths, secrets, actions.yaml loading
│  └─ internal/platform/         # screenshot, power, session helper, service
├─ web/                          # SvelteKit PWA (Tier 1 + 1.5)
├─ android/                      # Kotlin (Phase 3+)
├─ deploy/
│  ├─ install-windows.ps1        # elevated host bootstrap, idempotent
│  ├─ build.ps1                  # build agent + client, install to ProgramData
│  ├─ remote-mode.ps1            # arm / disarm / status
│  └─ install-linux.sh           # not written yet
├─ api/openapi.yaml              # single source of truth for both clients
└─ Taskfile.yml                  # cross-platform task runner (no Make on Windows)
```

`api/openapi.yaml` is not ceremony: it is what stops the Kotlin client and the PWA from drifting.
Generate the TS client and the Kotlin data classes from it.

---

## 4. Action registry format

Per-OS variants as data, with a portable default:

```yaml
- id: sleep-host
  label: Sleep
  icon: moon
  confirm: true
  run:
    default: { cmd: systemctl, args: [suspend] }
    windows: { cmd: rundll32.exe, args: ["powrprof.dll,SetSuspendState", "0,1,0"] }
    darwin:  { cmd: pmset, args: [sleepnow] }

- id: build-project
  label: Build Remoter
  cwd: "~/Desktop/Repos/Projects/Remoter/agent"   # ~ resolved portably
  run:
    default: { cmd: go, args: [build, ./...] }
  notify: on-complete                             # -> ntfy
  timeout: 15m
```

Rules baked into the loader:

- **Whitelist only. There is never an endpoint that runs an arbitrary string.** An agent with
  `POST /exec` is a remote shell on your tailnet wearing a JSON costume.
- `confirm: true` actions require a two-step POST: the first returns a short-lived nonce, the second
  replays it. Prevents a fat-thumb "Shut down" on a phone.
- No shell interpolation. If an action genuinely needs a pipeline, it points at a script file that
  lives in the repo and is reviewed like code.
- Unknown platform and no `default` → the action is listed as `unavailable`, not hidden. The UI
  should say why.

---

## 5. API contract (freeze this early)

```
GET    /api/actions              -> [{id,label,icon,confirm,available,reason}]
POST   /api/actions/:id/run      -> {jobId}   | 409 + {nonce} if confirm required
POST   /api/actions/:id/run      + {nonce}    -> {jobId}
GET    /api/jobs                 -> recent N
GET    /api/jobs/:id             -> {state,exit,started,ended,tail[]}
DELETE /api/jobs/:id             -> cancel (process-group kill)
GET    /api/screenshot?monitor=0&quality=60&scale=0.5  -> image/jpeg
GET    /api/monitors             -> [{index,w,h,primary}]
GET    /api/health               -> {version,os,uptime,tiers:{...}}
WS     /api/stream               -> job state deltas, log lines, agent events
```

- The WebSocket is the **only** live channel and it stays up in every tier (spec invariant, §1).
  Tier 3 entry/exit are just messages on it.
- Every response carries `X-Remoter-Bytes` so the client can maintain the per-tier data counter
  (§7, data-cap risk).
- Auth: bearer token in `Authorization`, generated at install, stored in the OS credential store
  where available (`wincred` / `libsecret` / Keychain) with a `0600` file fallback. Tailscale is the
  network boundary; the token is defence in depth, not the only lock.
- **Bind to the Tailscale interface plus loopback, explicitly. Never `0.0.0.0`** — there is a
  VirtualBox host-only network on this box and a coffee-shop Wi-Fi in its future.

---

## 6. Build order

Sized in evenings. Each phase has an explicit exit test — if it does not pass, do not start the next.

### Phase 0 — Off-the-shelf baseline · ~2 evenings

Prove the whole path works before writing anything. Concrete on this machine:

```powershell
# from the repo root. Self-elevates through UAC; idempotent, safe to re-run.
powershell -ExecutionPolicy Bypass -File .\deploy\install-windows.ps1
```

The script installs OpenSSH Server, Tailscale and Sunshine, applies the power and Wi-Fi settings
below, prints the `powercfg /a` sleep-state report, and lists what is left to do by hand. Note that
`Add-WindowsCapability` and the `powercfg` writes **require elevation** — shells on this box run
unelevated even though the account is in Administrators, which is why the bootstrap self-elevates
rather than assuming an admin prompt.

**Status, measured 2026-09-06 (post-reboot):**

| Item | State |
|---|---|
| Sunshine | Installed, `SunshineService` **Running** — encoder not yet configured |
| Tailscale | **Up**, node `laptop-a` = `<tailnet-ip>` — but see `ForceDaemon` below |
| OpenSSH Server | **Running.** Listening on `0.0.0.0:22` and `[::]:22` |
| Power settings | **Applied** — standby AC+DC `0`, lid-on-AC = do nothing |
| Wi-Fi power mgmt | **Not applied.** Driver limitation — see below |
| Sleep states | **Modern Standby confirmed:** `S0 Low Power Idle, Network Connected`. S1/S2/S3 all unavailable |

Three findings worth carrying forward:

1. **Tailscale is connected but not unattended.** `tailscale debug prefs` reports
   `WantRunning: true, LoggedOut: false` and **no `ForceDaemon`**. The tailnet is bound to the
   interactive session, so a reboot with nobody logging in still drops it. `tailscale up
   --unattended` remains outstanding — this is the spec's lockout trap, unmitigated.
2. **The Wi-Fi power toggle is not reachable via WMI on this hardware.** The MediaTek MT7921 driver
   exposes no `MSPower_DeviceEnable` instance (15 exist on the box; none belong to the NIC). The
   bootstrap now falls back to `PnPCapabilities = 24` in the driver's class key, which needs a
   reboot or adapter reset to take effect.
3. **`S0 Low Power Idle` is the "Network Connected" variant**, which is the better of the two — but
   Windows can still drop to disconnected standby under DTS/battery pressure. Disabling the standby
   timeouts prevents *automatic* entry; a lid close on battery or an explicit Sleep still gets there.
   Treat the 30-minute idle reachability test as mandatory, not optional.

**Security note:** sshd currently accepts connections on every interface, with the firewall rule
scoped only to the Private profile. Now that the tailnet exists, run the bootstrap with `-HardenSsh`
to scope inbound 22 to `100.64.0.0/10`. Deliberately opt-in: it costs you LAN SSH if Tailscale is
ever down.

- [x] ~~`winver`~~ → **Windows 11 Home. No RDP. Already answered — skip this step.**
- [ ] `tailscale up --unattended` — **the `--unattended` flag is the whole ballgame on Windows.**
      Without it the tailnet connection is bound to an interactive login and a reboot locks you out.
      This is the spec's "Tailscale starts after login" trap, and this flag is the fix.
- [ ] Verify from the phone on LTE, Wi-Fi **off**. Record the RTT — it is the Tier 3 latency budget.
- [x] **Power, laptop edition** — *applied by the bootstrap script; standby AC+DC verified `0`.*
      (Bigger than the spec's one-liner, because this is a notebook:)
      - `powercfg /a` — determine whether this is **Modern Standby (S0 low-power idle)**. If it is,
        "disable sleep" alone is not sufficient; also disable network disconnect in standby.
      - `powercfg /change standby-timeout-ac 0` **and** `-dc 0`, plus `hibernate-timeout-ac 0`.
      - Lid-close action on AC → *Do nothing*
        (`powercfg /setacvalueindex SCHEME_CURRENT SUB_BUTTONS LIDACTION 0`).
      - **Wi-Fi adapter → uncheck "Allow the computer to turn off this device to save power"**, and
        set the wireless profile's power-saving mode to Maximum Performance. On a laptop this is the
        single most likely cause of "it was reachable an hour ago".
      - Leave it on AC, and set a Lenovo Vantage battery conservation threshold (~60%) so a machine
        that lives plugged in does not cook its battery.
- [ ] ntfy: public `ntfy.sh` with a long random topic for now. **Payloads stay opaque**
      ("job 01J… done") — details get fetched over the tailnet. Self-hosting on the tailnet is nicer
      for privacy but costs a battery-draining foreground socket on the phone; revisit in Phase 1.
- [ ] Sunshine + Moonlight over Tailscale. Encoder `amdvce`, **HEVC**, bitrate capped at 4–6 Mbps.
      - **Laptop trap the spec does not cover:** with the lid closed and no external display there is
        no display to capture, and the stream dies. Either keep the lid open, or install a virtual
        display driver (Sunshine's virtual display / IddSampleDriver) and stream that.
- [ ] OpenSSH Server + Termius from the phone.
- [ ] Go stub with 3 real actions (see §1 deviation).
- [ ] **Measure the home uplink** (upload speed test). Tier 3 lives or dies on it, and it is the one
      number no amount of code will change.

**Exit test:** phone on LTE, Wi-Fi off, after a host reboot with nobody logged in — you can ping the
host, receive a push, SSH in, and start a stream. **Then use it for two weeks and write down what you
actually reached for.** Everything after this point is provisional until that list exists.

### Phase 1 — Agent + Tier 1 · ~4 evenings

**In progress.** The Go agent exists in `agent/` and runs. Verified working: bearer auth (401
without a token), `/api/health`, `/api/actions`, action execution with live output capture,
`/api/jobs/:id` tail, cancellation via process-tree kill, and the two-step confirm nonce
(409 + `{nonce, expiresIn}`, single-use, action-bound). `go vet` is clean and the binary
cross-compiles to `linux/amd64`, `linux/arm64`, `darwin/arm64` and `windows/amd64`.

The SvelteKit PWA exists in `web/` and is built and served **by the agent itself** — one origin, so
there is no CORS surface and no second port. Static assets are unauthenticated (the browser must
load the app before it can hold a token); everything under `/api/` stays behind the bearer token,
verified. `svelte-check` reports 0 errors. Whole client payload: **122 KB across 20 files**, which
is a one-time install cost, not a per-glance cost.

**Push is done and verified against a live topic.** Job completion publishes to ntfy.sh; the
round-trip was confirmed by polling the topic back, not just by a 200 from the POST.

Push policy is per-action and **silent by default** — a notification for every two-second command
trains you to ignore the channel. `notify:` accepts `on-complete`, `on-failure`, `always`; anything
else, including absent, means silence. Verified: a job without the key published nothing, a job with
it published exactly one message, and a cancelled job escalated from priority 3 to 4.

Payloads stay opaque (`"Build agent: cancelled"`) because the topic on a public server is only as
private as the guessability of its name. The topic is 24 random hex characters; detail is fetched
over the tailnet.

### Phase 2 — Tier 1.5 Glance · **done (capture); session split deferred**

`GET /api/screenshot?monitor=&quality=&scale=` returns JPEG, via `kbinani/screenshot` behind
`platform.Capture`. One implementation covers Windows and Linux; macOS needs cgo, so it falls
through to the stub and reports 501 rather than breaking the darwin cross-compile.

Measured on this host (2560×1600):

| quality | scale | payload |
|---|---|---|
| 60 | 0.50 | **63 KB** ← defaults |
| 40 | 0.50 | 51 KB |
| 80 | 0.50 | 86 KB |
| 30 | 0.35 | 26 KB |
| 60 | 1.00 | 184 KB |

Comfortably inside the spec's 50–200 KB band, and under the 150 KB target at defaults. Verified as a
real desktop image, not the black frame session isolation produces.

`/api/health` **probes** for glance rather than asserting it: the same binary can capture from a user
session and fail from session 0, so the honest answer depends on where it is running.

**The session-helper split is deliberately not built.** Because this host is not 24/7, the agent runs
in the user session as part of the armed mode, where capture simply works. The `CreateProcessAsUser`
helper only becomes necessary if the agent is ever promoted to a boot-time service — the code path
reports `ErrUnsupported` with "no active display" if that day comes, which is the signal to build it.

The client now has the tier switcher as a permanent bottom bar, with each tier labelled by its
bandwidth cost, and per-frame byte accounting on Glance.

**Because this host is not 24/7**, the agent is a component of the *armed* mode rather than an
always-on service — see `deploy/remote-mode.ps1`. Autostart in Phase 1 therefore means "started by
arming", not "Automatic at boot". This is a real departure from the original service-first design
and it simplifies Phase 2: there is no session-0 service to fight until Glance needs one.


- Go module, with `internal/platform` interfaces plus Windows/Linux/stub implementations from the
  first commit (retrofitting portability is the expensive way to get it).
- Actions registry loader with per-OS resolution and the whitelist rules (§4).
- Job runner: process groups so cancel actually kills children; ring-buffer log (cap ~1 MB/job);
  states `queued | running | done | failed | cancelled | timeout`.
- HTTP + WebSocket per §5. ntfy publish on terminal states.
- **Run as a Task Scheduler task at logon** ("run with highest privileges") for now — *not* a
  service. Simpler, and the session-0 problem does not bite until screenshots exist.
- SvelteKit PWA: action grid, job list, live log tail. Installable, dark, thumb-sized targets.

**Exit test:** the stub is deleted, two weeks run on the real agent, and the `GOOS=linux` build is
green.

### Phase 2 — Tier 1.5 Glance + the session split · ~3 evenings

This is where Windows actually fights back, and it is worth doing properly.

- `kbinani/screenshot` behind `platform.Capture(monitor)` → JPEG via `image/jpeg` at the `quality`
  and `scale` from the query. Target **under 150 KB** at `quality=60&scale=0.5`.
- **Session 0 isolation:** a LocalSystem service capturing the screen gets a black frame. Split the
  binary into two roles:
  - `remoter-agent` — service, session 0, owns the network and the job runner.
  - `remoter-agent --session-helper` — spawned into the active console session via
    `WTSQueryUserToken` + `CreateProcessAsUser`; does capture (and later input injection).
  - IPC over **loopback TCP with a per-launch shared secret** — portable, and on Linux/macOS the
    helper is simply the same process (`platform.NeedsSessionHelper()` → false).
- Now promote the agent from Task Scheduler to a real service via `kardianos/service`. Handle
  session-change notifications: the helper dies at logoff, respawn it on logon.
- Multi-monitor: `GET /api/monitors`, client picks, choice is remembered.

**Exit test:** locked screen, nobody logged in, phone on LTE — Glance returns a real image of the
lock screen; after a login it returns the desktop, with nothing restarted in between.

### Phase 3 — Native Android shell · ~5 evenings

- Kotlin, single Activity, Compose. Studio's bundled JDK; `compileSdk 36`, `minSdk 29`.
- The persistent mode switcher gets top-level UI (a bottom bar) — the switcher *is* the product.
- WebSocket in a **foreground service** with a low-priority notification; reconnect with exponential
  backoff and jitter; survive Doze (the socket will drop — reconnect fast rather than fight the OS).
- Generate the API client from `api/openapi.yaml`.
- Per-tier byte counters visible in the UI. On a metered plan, that number is a feature.

**Exit test:** cold start to a usable Tier 1 in under 2 s on LTE; after 30 minutes with the screen
off a push still arrives, and tapping it lands on the right job.

### Tier 2.5 "Live" — the rung the spec did not have

The ladder had a hole. Glance is one still at ~60 KB; Screen is Mbps. Nothing in between was usable
on mobile data, so in practice there was no interactive tier you would reach for casually.

**Live fills it: a low frame rate, scaled, tile-differenced desktop you can click.**
`GET /api/live` is a WebSocket carrying binary frames up and input events down.

The idea that makes it cheap is that a desktop is mostly static. Each frame is cut into 128 px tiles,
hashed, and only changed tiles are encoded and sent. Measured on this host at 4 fps, scale 0.4,
quality 35:

| | |
|---|---|
| First frame (keyframe, all tiles) | 46 KB |
| Steady state, idle desktop | **~2 KB/frame, one tile** |
| Effective rate | **~64 kbps** |

An idle screen sends nothing at all. A keyframe is resent every 60 frames so a lost tile heals.

Frames are binary, not JSON-wrapped: at a few frames a second an envelope per frame would be a real
share of the bandwidth this tier exists to save.

**Frame rate is capture-bound.** `StretchBlt` downscales inside GDI so only the scaled frame leaves
the graphics layer — at 0.4 scale that is 2.6 MB rather than 16 MB, and the separate CPU scaling pass
disappears. Measured back-to-back against the plain path on the same load:

| Path | Sustainable |
|---|---|
| Capture full + CPU downscale | 7.2 fps |
| `StretchBlt`, HALFTONE (≤10 fps) | 8.9 fps |
| `StretchBlt`, COLORONCOLOR (>10 fps) | 14.6 fps |

Above 10 fps the caller has asked for speed, so the coarser filter is used; below it text stays
antialiased. `TestLivePipelineBudget` measures both rather than assuming.

**The scale factor is passed into the platform layer, not computed by the caller.**
`GetSystemMetrics` reports DPI-scaled *logical* pixels — 1280×800 for this 2560×1600 panel — so a
caller sizing the target itself silently asks for half the resolution the slider promised, and
disagrees with Glance, which works in physical pixels.

**Tile hashes are committed only after the frame is on the wire.** This was the bug behind the first
attempt at this work, and it is subtle: the hashes are the server's model of *what the client is
currently displaying*. Updating them at encode time asserts delivery that has not happened. A failed
write — or a frame the client discards to stay inside its memory budget — then leaves those tiles
marked as delivered, and they are never resent until the next scheduled keyframe, up to 60 frames of
visibly stale screen. `encode` now stages, and `commit`/`discard` decide afterwards.
`TestDiscardedFrameResendsTiles` pins the behaviour.

The client closes the other half of the hole: when it drops a frame, or reconnects, it sends
`{"type":"resync"}` and the server answers with a keyframe. Throttled to once a second, because a
keyframe is expensive and a struggling link must not be asked for them continuously.

**On the first attempt, bandwidth was the real problem, not the pipeline.** The symptoms — worse
frame rate, broken sync, lost tiles, tiles painting in one by one — were blamed on the capture
changes and everything was reverted. The reverted state measured *slower*. The actual defects were
the premature hash commit above and a link that could not carry what was being asked of it.

**Input injection is new platform code** and follows the spec's §4 design, which until now was
reference material because Sunshine handled injection:

* `SendInput` only — never `keybd_event`/`mouse_event`, which cannot batch atomically.
* Characters go through `KEYEVENTF_UNICODE`, bypassing the host keyboard layout, so Hungarian text
  arrives without the host being set to a Hungarian layout.
* Named keys go through scancodes with the virtual key filled in, since DirectInput apps ignore
  VK-only events.
* Touch maps to mouse: tap to click, long press to right click.

Verified end to end, not just compiled: a `move` to (0.30, 0.70) put the real cursor at 384,560 — the
expected pixel. The `INPUT` struct layout is pinned by a test, because a wrong offset there does not
error, it just moves the cursor nowhere.

### Tier 3 removed from both clients

Screen was only ever "launch someone else's app", so it has been dropped from the web client and the
Android app. Sunshine and Moonlight remain installed and paired; they are simply used directly rather
than wrapped. The agent keeps `/api/screen` and the pairing broker, which are harmless and still
useful, but nothing in the UI depends on them.

Embedding Moonlight properly would mean vendoring `moonlight-common-a` with an NDK build (it is not
a Maven artifact), or reimplementing the GameStream protocol outright — pairing crypto, RTSP, RTP
with Reed-Solomon FEC, and an ENet control channel. That is months, and the success condition is
merely "as good as Moonlight". Live is the better use of the same effort, and unlike Moonlight it
works in the browser too.

### Phase 3 — Android app · **shipped, Tier 3 embedding deferred**

Kotlin + Compose in `android/`, built with Studio's bundled JDK 21 (the system JDK 23 is not an
AGP-supported version). Published by `build.ps1 -Android` to `/remoter.apk`, so the phone installs it
straight off the tailnet.

What the app adds over a browser bookmark — and these are the only two things that justify it:

* **The token is sealed by the Android Keystore** (`EncryptedSharedPreferences`), not sitting in
  `localStorage` as plaintext.
* **A foreground service owns the Tier 1 socket**, so job events keep arriving with the phone in a
  pocket. A WebView's socket dies on backgrounding — exactly when a long job finishes.

**Tier 3 works here and could not in the browser.** `getLaunchIntentForPackage` starts Moonlight
directly; the `BROWSABLE` restriction is a browser rule, not an app one.

**Tier 2 is the agent's own xterm.js console in a WebView**, not a hand-written emulator. PowerShell
with PSReadLine drives a lot of VT — colour, cursor addressing, bracketed paste — and a partial
emulator renders that wrongly in ways that are hard to spot and worse than useless at a distance.
Termux's terminal widgets were the obvious alternative, but their `TerminalSession` forks a local
process over JNI and has no path for a stream arriving on a socket. The token is handed over in a
one-shot query parameter that the page consumes and scrubs from history.

**Embedded Moonlight is not done, and is not a small job.** `moonlight-common-a` is not published as
a Maven artifact — it is a git submodule with JNI native code inside the Moonlight app, so using it
means vendoring the source and adding an NDK build. That is the spec's own two-week estimate and is
tracked as future work rather than half-built.

**Still outstanding: TLS.** The transport is cleartext HTTP inside the WireGuard tunnel, documented
as such in `network_security_config.xml`. The "embedded encryption" goal means a self-signed
certificate on the agent pinned by the app, giving encryption independent of Tailscale.

### Privilege — the agent runs unelevated

Arming needs elevation (power schemes, services), and a process started from an elevated shell
inherits that elevation. So the agent was running as admin — and because Tier 2 hands out a shell,
that meant **anyone holding the bearer token got an admin shell**.

Fixed by starting the agent through a scheduled task registered with `RunLevel = Limited`
(`deploy/install-task.ps1`), which breaks the inheritance. Verified end to end: `/api/health` reports
`elevated: false`, and a shell opened over Tier 2 reports `ADMIN_SHELL=False`.

`/api/health` now reports `elevated` at all, so the privilege the token grants is visible rather than
assumed. `remote-mode.ps1 -Status` distinguishes *reported false* from *not reported* — guessing
"normal" about privilege is the wrong way to be wrong.

Side benefits: an unelevated `build.ps1` can stop the agent again (redeploys no longer fail on a file
lock), and the task's Interactive logon type matches Glance's need for a desktop session.

Two bugs surfaced while doing this, both now fixed:

* A stale `agent.log` written by the earlier elevated run denied write to the unelevated task, and
  the agent **exited** rather than start. A logging problem must never make the host unreachable —
  that is the exact failure this project exists to prevent — so it now degrades to stderr and carries
  on. `install-task.ps1` also rotates an unwritable log aside.
* The failure was invisible because a scheduled task discards stderr and the agent died before
  opening its log. `LastTaskResult` was the only evidence.

### Phase 4/5 status — **done in the web client**

Reordered deliberately: Console and Screen ship before the Android app, because the tiers are the
product and the wrapper is not. The Android app is now last, and its job is to mirror this UI with
stricter security and embedded encryption rather than reimplement it.

**Tier 2 (Console).** `GET /api/console` is a WebSocket carrying a real PTY: JSON control frames up
(`data`, `resize`), raw bytes down, rendered by xterm.js.

*This departs from the spec's "use SSH, do not write a PTY server".* That advice assumed a native
client. A browser cannot speak SSH, and the alternative — making the agent an SSH client — would mean
it holding a key that grants a full shell, which is worse than owning the PTY directly. `go-pty`
wraps ConPTY on Windows and forkpty elsewhere, so this is still one implementation, not a
Windows-only feature. Verified against real PowerShell with ANSI colour, PSReadLine highlighting and
resize.

Console is a **full shell**, strictly more powerful than the Tier 1 whitelist. It is therefore
switchable (`console.enabled` in config), announced at startup, and logged on open/close.

**Tier 3 (Screen).** Handoff, as the plan recommended: `GET /api/screen` TCP-probes Sunshine's port
and reports whether it is genuinely accepting connections, plus the address the client reached the
agent on — never a guessed interface. No encoder was written.

**A browser cannot launch Moonlight.** The spec's "fire an Android Intent into Moonlight" works from
an *app*, which may start any exported activity. It does not work from a *browser*: Chrome only
follows an `intent:` URL whose target activity declares `android.intent.category.BROWSABLE`, and
Moonlight's launcher activity does not. Every attempt therefore falls back — to the Play Store,
telling you to install an app you already have. There is no package name or intent spelling that
fixes this. The launch button was removed; the Screen tier hands over the host address instead.

This sharpens the case for the Android client: it is the only place Tier 3 can stop being a seam,
both because it can launch Moonlight by intent and because it can embed `moonlight-common-a` outright.

**Pairing is brokered by the agent.** Sunshine defaults to `origin_web_ui_allowed=pc` and blocks
non-localhost origins with a CSRF error — which is what a phone pointed at its web UI hits.
`POST /api/screen/pair` forwards the PIN from loopback, the origin Sunshine trusts, so its **admin
UI never goes on the network**. Sunshine credentials are optional in config and preferred per
request, since pairing is rare and they are admin credentials.

Also confirmed on this host: `Found H.264 encoder: h264_amf` and `Found HEVC encoder: hevc_amf`,
`av1_amf` unsupported — exactly as the survey predicted. The ViGEmBus warning Sunshine reports as
"Fatal" affects **gamepad emulation only**; mouse and keyboard use `SendInput` and need no driver.

**Bundle cost.** xterm.js is ~326 KB, more than three times the rest of the app. Loading it for
someone who only taps Tier 1 buttons would contradict the premise, so it is dynamically imported when
Console is first opened. Initial JS payload stays **73.8 KB**; the terminal chunk is fetched on
demand. The tier ladder applies to the client bundle, not just to the wire.

### Phase 4 — Tier 2 Console · ~3 evenings

- Embed a terminal view over SSH. The **modifier bar is built here, once**, and reused by Tier 3 —
  sticky Ctrl/Alt/Shift/Tab/Esc/arrows, per the spec's §4.
- Key handling follows the spec's two-path design from the start, even in Tier 2: printable
  characters through the Unicode path, modifiers through the key-event path. Building it once is the
  whole point of doing it here.
- Host side: nothing. OpenSSH is already portable and already installed.

**Reality check before building:** if two weeks of Phase 0 showed Termius covers this, **ship nothing
here** and mark the tier "handled by Termius" in the mode switcher. That is a legitimate answer to
the spec's open question, not a cop-out.

### Phase 5 — Tier 3 · handoff ~1 evening, embedded ~2 weeks

- **Handoff first.** An Android Intent into Moonlight, with the tier switcher tearing down the Tier 3
  UI on return. One evening, zero video code, and it tells you whether the seam actually bothers you
  in daily use — which is the only way that question gets answered honestly.
- **Embedded later**, via `moonlight-common-a`. Only justified once everything else is solid: this is
  the phase that makes the project *the* product rather than a launcher, and also the phase most
  likely to eat a month.
- Either way: **tear the pipeline fully down on tier exit; never pause.** A backgrounded stream is a
  data plan. Assert it — the byte counter should flatline within 2 s of leaving Tier 3, and that is a
  test, not a hope.

The spec's §4 input design applies only to the embedded path *and* only if Sunshine is replaced on
the host. It will not be. Keep §4 as reference, not as work.

---

## 7. Risk register

| Risk | Why it bites *here* | Mitigation |
|---|---|---|
| Laptop unreachable | Wi-Fi power saving + Modern Standby, no Ethernet, no usable WoL | Phase 0 power block; agent heartbeat to ntfy so silence is noticeable |
| Lid closed → no display → no stream | Notebook host with no external monitor | Virtual display driver, or the lid stays open |
| Session 0 black screenshots | Service + capture is a Windows-only footgun | Session-helper split, Phase 2 — designed for, not discovered |
| Windows Home limits | No RDP, no gpedit, no Local Users & Groups | Already priced in; nothing in the plan depends on them |
| Non-ASCII output mangled | Hungarian text through a legacy console codepage | Force UTF-8 and validate (§2, rule 5) |
| Data cap | Tier 3 on LTE is Mbps | Hard teardown, visible byte counter, HEVC, capped bitrate |
| Portability rot | Only one host exists, so nothing forces the discipline | CI cross-compile matrix from commit 1 (§2, rule 4) |
| Scope creep into video | Phase 5 embedded is a month-shaped hole | Handoff first; embedded only after Tiers 1–2 have real daily use |
| The agent is an RCE surface | It runs commands as an admin user, reachable from a phone | Whitelist-only registry, no `/exec`, confirm nonces, tailnet-only binding, bearer token |

---

## 8. Licensing

Unchanged from the spec, with one consequence worth stating: skipping OliveTin (§1) means **no AGPL
code enters the tree at all**. Tailscale/Headscale are BSD, ntfy is Apache-2.0/GPLv2, and
Sunshine/Moonlight stay at arm's length as separately-installed binaries you talk to over a socket or
an Intent — not linked, not vendored. If this ever ships, that boundary is already clean. Keep it
that way: nothing GPL gets vendored into `agent/` or `android/`.

---

## 9. Open questions — status

| Spec question | Status |
|---|---|
| Windows Pro or Home? | **Answered: Home.** No RDP fallback exists. |
| Which actions do you actually want? | Open by design — answered by two weeks of Phase 0. |
| Is Glance the tier you use most? | Open. If yes, spend the Phase 4/5 budget on Glance instead: diff regions, change-triggered pushes, OCR of dialog text. |
| Is Tier 2 needed at all? | Open. Termius may already close it — see Phase 4. |
| Multi-machine? | Deferred, but **not designed out**: the agent is per-host and the client's connection list is a list from commit 1. Cheap now, expensive later. |
| Self-hosted vs public ntfy? | New question. Public + opaque payloads for Phase 0–1; revisit against phone battery cost. |
