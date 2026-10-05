# Hub Architecture — hubbox as jump server

Supersedes the direct client→agent topology in [IMPLEMENTATION-PLAN.md](IMPLEMENTATION-PLAN.md).
The tiers, the agent's capture/input/console code and the clients' renderers carry over; what
changes is *who talks to whom*, and *who trusts whom*.

Decided 2026-10-03, by interview. Every row in §2 was an explicit answer, not a default.

---

## 1. Topology

```
   controllers                         hub                          targets
┌──────────────┐               ┌──────────────────┐          ┌──────────────────┐
│ Android app  │──┐            │ hubbox (Debian)  │    ┌────▶│ laptop-a (Win11) │
│ (also target)│  │  HTTPS     │ remoter-hub      │    │     │ remoter-node svc │
└──────────────┘  ├──────────▶ │  · who-is auth   │────┼────▶│ laptop-b (Win)   │
┌──────────────┐  │  :443      │  · PIN gate      │    │     └──────────────────┘
│ Laptop       │──┘            │  · relay /d/{id} │    ├────▶ phone (Android)
│ browser      │               │  · presence/push │    │
└──────────────┘               │ remoter-node     │◀───┘ (hubbox itself, loopback)
                               └──────────────────┘
        ═══════════════════ all over Tailscale (<tailnet>.ts.net) ═════════════════════
```

* Controllers talk **only** to the hub, at `https://hubbox.<tailnet>.ts.net`.
* Targets accept connections **only** from the hub (`<hub-ip>`).
* Every byte of every tier — Actions, Glance, Console, Live — is relayed through hubbox.
* A device can be both: the Android app controls *and* is controllable; a laptop is a target
  through its node and a controller through its browser.

## 2. Decisions

| Question | Decision | Consequence |
|---|---|---|
| Data path | **Relay everything via hubbox** | One extra hop (~1 ms on the home LAN). Away targets cost home bandwidth twice; Live is 64 kbps–2 Mbps, so acceptable. |
| Targets | **Windows laptops, hubbox, Android phone** | Three node implementations (§4). |
| Hub down | **Hard dependency** | Nothing is reachable without hubbox. In return, targets are closed to everything else. |
| Windows unattended | **Always, incl. lock screen and after reboot** | Boot-time service + session helper (§4.1). Live can click UAC prompts — that is inherent to lock-screen access. |
| Controller auth | **Tailscale identity + device approval + PIN** | No tokens to type. PIN guards Glance, Live, Console. |
| PIN cadence | **Per session, 5-minute grace** | Fingerprint may stand in on the phone. |
| Android as target | **Both modes** | Accessibility (unattended, ≤1 fps) by default; MediaProjection upgrade when someone taps *Start now*. |
| Session indicator | **Visible, no approval** | Tray icon / notification while a session runs. |
| Sleep | **Remote arm / disarm** | Arming keeps a laptop awake; auto-disarm on timer, low battery, or local use (§6). |
| Sunshine / Moonlight | **Dropped** | Live (with 1:1) is the top tier. Targets accept nothing but hubbox. |
| Hub deployment | **systemd, native binary** | Console on hubbox is hubbox's real shell; the hub reaches tailscaled's LocalAPI directly. |
| Push | **Job finished, session started, armed device offline, new device waiting** | ntfy, published only by the hub. |
| TLS | **Tailscale certificate** for `hubbox.<tailnet>.ts.net` | Browsers get a secure context (PWA install, clipboard). |
| Build | **Phased** (§9) | Each phase is usable on its own. |
| Deploy | **Claude deploys to hubbox over SSH** | SSH hardening: see §8. |

## 3. The hub — `remoter-hub` on hubbox

One Go binary, cross-compiled `linux/amd64` from the Windows box (hubbox has no Go toolchain and
needs none). Runs as a systemd service under a dedicated `remoter` user; state in
`/var/lib/remoter` (0700).

**Listening.** `<hub-ip>:443` only — never `0.0.0.0`, same rule as the agent. TLS
certificate fetched from tailscaled's LocalAPI (`/localapi/v0/cert/…`), which also renews it.
The hub's own DNS name is read from tailscaled at startup, never hardcoded, so a tailnet rename
needs no code change.
Talking to the LocalAPI over its unix socket directly, rather than importing `tailscale.com`, keeps
a very large dependency tree out of the binary. The `remoter` user needs
`TS_PERMIT_CERT_UID=remoter` in `/etc/default/tailscaled`.

**Who is calling.** Every request's remote address goes to `/localapi/v0/whois`, which returns the
Tailscale *node* (device), not just the user. All devices here belong to one Tailscale user, so the
node is the identity that matters. Cached per address for 30 s.

**Approval.** A device unknown to the hub gets a *waiting* page and a push to the owner. Approval
happens from an already-approved device, or — for the very first one — `remoter-hub approve <name>`
over SSH. Revocation is one click, and removing a device from Tailscale revokes it implicitly.

**PIN.** Stored as an argon2id hash. Required to open Glance, Live or Console. Enforced **on the
hub**, never trusted from the client: a grant is bound to the controller's node, stays valid while
any guarded session is open, and for 5 minutes after the last one closes. On the phone the PIN is
sealed under a biometric-bound Keystore key, so a fingerprint releases it. Set with
`remoter-hub set-pin`, or on first use from the first approved device.

**Relay.** `/d/{deviceId}/api/...` → `http://{node}:8737/api/...`, through
`httputil.ReverseProxy` (handles WebSocket upgrades natively). The hub strips the controller's
credentials and attaches the node's secret. Byte accounting (`X-Remoter-Bytes`) passes through.

**Presence.** Heartbeat to every node every 30 s; two misses = offline. Offline is only *news*
(push) for an **armed** device — an unarmed laptop asleep is normal.

**Also serves** the web client and `remoter.apk`, as the agent does today, and keeps an audit log
of every session open/close (who, which device, which tier, duration).

## 4. The node — the "very small service"

The current agent becomes the node. **Idle, it is a listener and a heartbeat and nothing else**;
capture, input and shells start on the first request and are torn down 30 s after the last
controller leaves.

Common to every platform:

* Accepts connections only from `<hub-ip>` — enforced by the OS firewall *and* checked in
  the app — plus a per-node secret issued at enrollment. Defence in depth, not either/or.
* Enrollment: the installer registers with the hub, the node appears as *waiting*, the owner
  approves it — the same flow as a controller.
* Several controllers may hold a session on one target at once; all can control. The indicator
  lists who is connected.

### 4.1 Windows

One binary, three roles:

| Role | Runs as | Where | Does |
|---|---|---|---|
| `service` | LocalSystem | session 0, at boot | network, heartbeat, arming, spawns the helpers |
| `screen-helper` | SYSTEM token, re-sessioned | active console session | capture + input; follows the *input desktop*, so it sees and drives the lock screen and UAC prompts |
| `user-helper` | logged-on user (`WTSQueryUserToken`) | user's desktop | Console PTY, Actions, tray indicator |

* Console and Actions run with **the logged-on user's privilege**, exactly as today's unelevated
  agent does. With nobody logged on they are unavailable — log in through Live at the lock
  screen first. A SYSTEM shell is never handed out.
* Helpers are respawned on session change (logon, logoff, fast user switch).
* IPC service ↔ helpers: loopback with a per-launch secret, as IMPLEMENTATION-PLAN §6 Phase 2
  designed. This is that phase, finally built.
* Replaces `remote-mode.ps1` and `install-task.ps1`. Arming no longer needs UAC: the service
  already holds the privilege to switch power schemes.

### 4.2 hubbox (Linux)

A second `remoter-node` process under its own systemd unit, `User=<user>`, on
`127.0.0.1:8737`. The hub relays to it like any other node. Console + Actions only: hubbox has no
desktop session, so Glance and Live report unavailable rather than pretending.

### 4.3 Android

Lives inside the existing app; the target side is opt-in per phone.

* **Unattended (default):** an AccessibilityService — `takeScreenshot` (rate-limited by Android to
  about 1/s), `dispatchGesture` for taps and swipes, global actions for Back / Home / Recents,
  `ACTION_SET_TEXT` for typing.
* **Attended upgrade:** MediaProjection for smooth video. Android requires a *Start now* tap on the
  phone (each session on Android 14+).
* No Console or Actions — an app sandbox shell is not useful.
* Listener in a foreground service. HyperOS needs *Autostart* on and battery set to *No
  restrictions*, or it will kill the listener.

## 5. Clients

* **Android:** pairs with the hub once (approval, no token). The machine list comes from the hub;
  the per-host token list built last session is retired, along with direct mode.
* **Browser on a laptop:** `https://hubbox.<tailnet>.ts.net`, device picker on top of today's tiers.
  1:1 mode stays.
* All API paths gain the `/d/{deviceId}` prefix; the tiers themselves are unchanged.

## 6. Arming

Arm / disarm from the hub, per device. Implemented **on the node**, so an armed laptop disarms
itself correctly even while hubbox is down.

Armed = awake on AC and battery, lid close does nothing (the existing "Remoter Armed" scheme).
Auto-disarm on any of:

* **Timer** chosen at arm time (optional).
* **Low battery:** on battery and below 20%. Sleeping beats dying.
* **You're back:** physical keyboard or mouse input while no session is active. Injected input is
  told apart from physical input (raw-input device handle), so Live never disarms its own target.

Each auto-disarm pushes a notification saying why. hubbox itself is a server and is never "armed".

## 7. Removed

* Sunshine / Moonlight: `/api/screen`, `/api/screen/pair`, `tiers.screen`, the Sunshine probe, and
  `SunshineService` in arming. Uninstalling Sunshine from the laptops is left to the owner.
* Direct client → agent connections, per-host bearer tokens in the clients.
* `deploy/remote-mode.ps1`, `deploy/install-task.ps1` (replaced by the node installer).

## 8. hubbox host changes

* **HTTPS certificates** must be enabled in the Tailscale admin console (DNS → HTTPS
  Certificates). Currently off: `CertDomains` is empty.
* **SSH:** key-only from the tailnet; password still accepted from the home LAN
  (`Match Address <lan-subnet>`) so `ssh <user>@hubbox.local` keeps working without a key.
  Verified preconditions: avahi is active, no tailnet node advertises a subnet route (so tailnet
  traffic cannot arrive with a LAN source address), Docker bridges are outside the match.
  **Blocking condition:** the weak initial password must be changed first — with LAN password login,
  every device on the Wi-Fi is one guess from root on the machine that controls all the others.

## 9. Phases

Each ends with an exit test; do not start the next until it passes.

**Phase 1 — hub + Windows node + clients.**
Hub on hubbox (who-is, approval, PIN, relay, presence, push, audit). Windows node with service,
screen-helper and user-helper. Remote arming with all three auto-disarms. Web and Android
switched to the hub. Sunshine removed.
*Exit:* reboot laptop-a, nobody logs in. From the phone on LTE, via hubbox: see the lock screen in
Live, type the password, reach the desktop, open Console as the user. A session-started push
arrives. A non-approved device gets the waiting page and nothing else.

**Phase 2 — hubbox as a target.**
`remoter-node` as `<user>` on loopback; Console + Actions.
*Exit:* a Console on hubbox from the phone, as `<user>`, through the same hub UI.

**Phase 3 — Android as a target.**
Accessibility mode, then the MediaProjection upgrade.
*Exit:* from laptop-a's browser, unlock-free control of the phone in a drawer at ~1 fps; with a
*Start now* tap, smooth video.

## 10. Status — Phase 1, 2026-10-05

**Built and verified end to end, through hubbox:**

| Piece | Verified |
|---|---|
| Hub on hubbox | systemd `remoter-hub` as user `remoter`, HTTPS on `<hub-ip>:443` with the Tailscale cert. Unknown devices get *pending* and nothing else; cross-origin, cross-site and foreign-`Host` requests are refused. |
| Identity + approval | who-is identifies `laptop-a` by stable node ID; `remoter-hub approve` over the admin socket; approval and revocation from the Access page. |
| PIN | argon2id, server-enforced on Glance/Live/Console and approvals; 5-minute grace; lockout after 5 wrong attempts (unit-tested). |
| Relay | Actions, jobs, Glance, Live and Console relayed with per-node tokens; a direct connection to a node from anywhere but hubbox gets 403. |
| Windows node | `RemoterNode` service (LocalSystem). Screen helper (SYSTEM, console session) serves Glance/Live; user helper runs **unelevated as the signed-in user** (`whoami` → `laptop-a\<user>`) for Console and Actions. |
| Audit + push | Session open/close with duration, approvals, PIN events, action runs. ntfy pushes on the existing topic. |
| Clients | Web client rewritten for the hub (device picker, arming, PIN, Access page). Android app rewritten for the hub (device list, arming, PIN with optional fingerprint unlock, Access page); release build is R8-minified to 3.1 MB. |
| Arming | Arm switched laptop-a to the "Remoter Armed" scheme, survived a service restart and a reboot, and disarm restored Balanced. The low-battery rule fired on its own (unplugged, under 20%) and restored Balanced. |
| Lock screen | With laptop-a locked, Glance through hubbox returned the lock screen. A click over Live reached the lock screen's UI, a Space lifted the curtain, and capture followed Windows back to the desktop after sign-in. |
| Phone app | `phone` approved; PIN, Glance, Actions, Console and Live used from the phone through the hub (audit log, 2026-10-05). |
| Session indicator | A click-through, always-on-top banner ("● Remote session · laptop-a (Console)") at the top of the target's screen while anyone is connected, plus a tray icon. The banner exists because Windows 11 hides new tray icons in the overflow, and Do Not Disturb swallows toasts. |

**Not yet verified:**

- ~~The full exit test~~ **passed 2026-10-05, 16:01:** armed, rebooted, untouched; reachable through
  the hub at +41 s while the session was still locked, and the phone reopened Live by itself. That
  boot, Wi-Fi happened to connect promptly (+28 s), so the Wi-Fi safety net (built, not yet
  installed on laptop-a - its UAC prompt went unanswered) was not exercised. It covers the slow boots.
- The "owner is back" and timer auto-disarm rules (low battery is verified; they share its code path).

**Reboot recovery** (after the first reboot test failed, 2026-10-05): the node came back 46 s after
boot, but clients kept showing the device as offline. Fixed by: the node calling `/hub/hello` the
moment its tailnet listener binds; the hub polling offline nodes every 5 s instead of 30 s; clients
polling every 3 s while the selected device is away; the app showing "offline, reconnecting"
in place of the tier, retrying failed page loads by itself, and destroying WebViews it drops (each
one had kept a Live socket running); and WebSocket pings on Live, Console and the job stream, so a
vanished client no longer holds a session open.

**Known limits:**

- Every node update needs one UAC prompt: the service binary lives in Program Files, where only
  admins can write, by design.
- `deploy-hub.ps1` asks for hubbox's sudo password interactively (`ssh -t`).
- With the lid closed and no external display there is no screen to capture (unchanged from
  before; a virtual display driver is the fix).

**Phase 1 blocker found by the reboot test:** laptop-a's Wi-Fi does not connect until someone unlocks
the screen. After a reboot on 2026-10-05, Windows signed in automatically at +15 s, but WLAN
AutoConfig only started connecting at +87 s, eight seconds after the session was unlocked, even
though the home network's profile is All-User and set to connect automatically. The node was
ready at +17 s and reached the hub 7 s after Wi-Fi came up, so the gap is the network, not
Remoter. Research and logs ruled out the lock screen (one boot connected while locked), Kernel DMA
Protection (Off; the card starts 8 s after boot) and the router (channel 36, not DFS). Across boots
the wait varies from 27 s to over 4 minutes with no attempt logged, which points at the adapter's
2022 driver (MediaTek MT7921, 3.0.1.1309).

**Safety net (built 2026-10-05):** the node service watches Wi-Fi through the Native Wifi API
(`wifi_windows.go`; `netsh` output is localised). If Wi-Fi has been down for 20 s within the first
10 minutes after boot, or at any time while armed, it asks Windows to connect: first to the network
it last saw connected (remembered in `wifi.json`), then to other saved, secured networks in range,
one every 30 s. Outside those windows it never acts, so a deliberate disconnect is left alone.
Covered by unit tests; on-device proof is the next reboot (look for "asking Windows to connect" in
`node.log`).

## 11. Status — Phase 2, 2026-10-05

hubbox is a target. `remoter-node` runs as `<user>` under systemd (`remoter-node.service`), listening
on **127.0.0.1:8737 only**: the hub recognises a node on its own machine at enrolment and dials it
over loopback, so nothing on the tailnet can reach it at all. Installed by `deploy/install-node.sh`
(which `deploy-hub.ps1` now runs too). Because hubbox's DNS does not go through Tailscale (it has its own resolver),
the installer pins the hub's own name to hubbox's tailnet IP in `/etc/hosts`.

Verified through the hub: health (mode standalone, user `<user>`; Glance/Live reported unavailable,
no desktop; arming not offered); Actions from `deploy/actions-hub.yaml` run as `<user>` (the
container list came back); restarts stop at the confirm step; a direct connection to
`<hub-ip>:8737` from the tailnet is refused. Console on hubbox (a bash shell as `<user>`,
behind the PIN) **verified 2026-10-05**. Phase 2 is done.

## 12. Status — Phase 3, 2026-10-05 (unattended mode built, not yet on the phone)

The Android app is now also a node (`android/.../node/`):

* `NodeService`: foreground service (`specialUse`; `dataSync` is capped at 6 h/day on Android 15)
  running a NanoHTTPD + WebSocket server on the **tailnet address only**, port 8737, answering the
  hub's address only, with the token generated on the phone. It re-binds when Tailscale's address
  changes, says hello to the hub, restarts at boot, and its notification is the session indicator.
* `PhoneControl`: an Accessibility service - screenshots (`takeScreenshot`, API 30+, rate-limited
  by Android to a few per second), gestures (tap, long press, swipe), global actions (Back, Home,
  Recents, notifications), and typing by rewriting the focused field.
* `TileCoder`: the Live tile protocol, byte for byte the same as `live.go`, so the existing viewers
  render a phone unchanged.
* The viewer knows when the target is a phone: a one-finger drag becomes a swipe, and the key row
  offers Back / Home / Recents / Notifications.
* "Control this phone" (app menu) walks through Accessibility (including Android's "Allow
  restricted settings" for sideloaded apps), the battery exemption, HyperOS Autostart and enrolment.

Health reports Glance and Live only (no Actions, no Console; arming is not offered). Not yet done:
the attended MediaProjection upgrade for smooth video, and on-device verification.

**Phase 3 additions (2026-10-05):**

* **Smooth mode.** The viewer's *Smooth* button asks the phone for a MediaProjection;
  `ProjectionActivity` shows Android's "Start recording or casting?" prompt (entire screen only, on
  Android 14+), launched through the Accessibility service, which is allowed to open activities from
  the background. While a projection runs the node service adds the `mediaProjection`
  foreground-service type, which Android 14 demands, and drops it again afterwards. Frames come from
  a VirtualDisplay at the target size (one display per projection, resized for scale changes) at up
  to 10 fps; the session falls back to screenshots when the projection is stopped from the status
  bar, and ends it when the viewer leaves. The node reports each step to the viewer as a JSON text
  message on the Live socket.
* **Hold and drag.** A finger that rests 500 ms and then moves sends `drag`: a path with a hold
  first. On a phone that is a held stroke continued into the movement (icons, text handles,
  sliders); on Windows it is a real mouse drag (`live.go`), which also makes a plain one-finger drag
  on a PC a mouse drag. Resting and lifting is the long press / right click. A sticky *Hold* button
  makes the next touch start held.

Both **verified on the phone 2026-10-05** (smooth Live after "Start now"; hold-and-drag). The Windows
mouse drag ships with the next laptop-a node install, together with the Wi-Fi safety net.
