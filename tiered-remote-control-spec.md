# Tiered Remote Control — Project Spec

**Goal:** reach a home Windows PC from an Android phone over mobile data, escalating from cheap
status/actions up to a full interactive desktop stream only when needed.

**Core idea:** every existing remote tool starts you at full video. This one starts at kilobytes
and lets you climb. The mode switch is the product; the streaming is a commodity.

---

## 1. Tier model

| Tier | Name    | Payload                          | Bandwidth   | Use case                                |
|------|---------|----------------------------------|-------------|-----------------------------------------|
| 1    | Actions | JSON over WebSocket              | ~KB         | Kick off a job, check status, tail a log |
| 1.5  | Glance  | Single JPEG screenshot on demand | ~50–200 KB  | "Is a dialog waiting for me?"           |
| 2    | Console | PTY stream (SSH / ttyd)          | ~10s KB/s   | Real terminal work                      |
| 3    | Screen  | H.264/HEVC video + input inject  | Mbps        | GUI apps, browser                       |

Default entry point is Tier 1. Escalation is always explicit and user-initiated.

**Invariant:** the Tier 1 WebSocket stays connected in all modes. Mode switching changes what is
rendered, not what is connected. Notifications and job status keep flowing while streaming.

---

## 2. Architecture

```
┌─────────────────────── Android app ───────────────────────┐
│  Tier 1/1.5 UI  │  Tier 2 terminal  │  Tier 3 video+input │
└────────┬────────────────┬───────────────────┬─────────────┘
         │ WSS            │ SSH               │ UDP (ENet)
         │                │                   │
    ═════╪════════════════╪═══════════════════╪══════ Tailscale (WireGuard)
         │                │                   │
┌────────┴────────────────┴───────────────────┴─────────────┐
│  Agent (Go/Node)   OpenSSH Server      Sunshine (service) │
│  - action registry                     - NVENC/AMF/QSV    │
│  - screenshot endpoint                 - SendInput helper │
│  - ntfy publisher                                          │
└────────────────────── Windows host ────────────────────────┘
```

### Network layer — decided
**Tailscale** on both ends. Solves NAT traversal and carrier CGNAT, gives the host a stable IP.
No port forwarding, no exposed RDP. Headscale later if you want to drop the dependency on
Tailscale's coordination servers.

### Push layer — decided
**ntfy**, self-hosted or public. HTTP pub-sub, Go single binary, open-source Android client.
Do not build push. `curl -d "build done" http://host:8080/topic` and move on.

### Tier 3 host — decided
**Sunshine.** Hardware encode, already runs as a Windows service (which is how it survives UAC
and session isolation), already injects mouse/keyboard/gamepad. Do not write an encoder.

---

## 3. Build order

### Phase 0 — Off-the-shelf baseline (1 evening)
Prove the concept before writing anything.

- [ ] `winver` → confirm Windows Pro vs Home (decides whether RDP is available as a fallback)
- [ ] Tailscale on PC + phone; verify ping from LTE with WiFi off
- [ ] `powercfg /change standby-timeout-ac 0` — sleep is the #1 failure mode
- [ ] Set Tailscale service to start before login (otherwise a reboot locks you out)
- [ ] ntfy server + Android app; send yourself a test push
- [ ] Sunshine + Moonlight; confirm stream and input work over Tailscale
- [ ] OliveTin (WSL or container, shelling to PowerShell over SSH) with 3 real buttons
- [ ] Enable Windows OpenSSH Server; connect with Termius

**Then use it for two weeks.** The gaps you hit are the actual spec. Everything below is
provisional until that happens.

### Phase 1 — Agent + Tier 1
Replaces OliveTin with something you control.

- Go or Node service on the host, runs as a Windows service
- YAML/JSON action registry: `{ id, label, icon, command, args[], confirm }`
- `GET /actions`, `POST /actions/:id/run`, `GET /jobs/:id` (status + tail)
- WebSocket push of job state changes to connected clients
- On job completion → publish to ntfy
- SvelteKit web UI first. Mobile browser is a fine Tier 1 client and skips the app store loop.

### Phase 2 — Tier 1.5 Glance
Cheapest big win. `GET /screenshot?quality=60&scale=0.5`, `System.Drawing` or `windows-capture`,
JPEG out. Multi-monitor: return an index, let the client pick.

### Phase 3 — Native Android shell
Now wrap it. Kotlin, single Activity, three fragments/composables + a persistent mode switcher.
Tier 1 UI ports over from the SvelteKit thinking. Keep the WebSocket in a foreground service.

### Phase 4 — Tier 2 Console
Embed a terminal emulator (`Termux:terminal-view` or `libtermux` are the usual bases) over SSH.
Modifier bar from Phase 5 gets reused here — build it once.

### Phase 5 — Tier 3
Two options, pick based on how much you care about the seam:

- **Handoff:** fire an Android Intent into Moonlight. One day of work. Visible seam, zero video code.
- **Embedded:** pull in `moonlight-common-a` (Java, handles pairing/control stream/decode setup).
  Real toggle, one activity. This is the version that justifies the project existing.

Start with handoff. Move to embedded once everything else is solid.

---

## 4. Input design (Tier 3, embedded path only)

Sunshine handles injection if you use it. This section applies only if you replace the host side.

**API:** `SendInput`. Never `keybd_event` / `mouse_event` — deprecated, no atomic batching.

**Two keyboard paths:**

| Input                          | Path                                      | Why |
|--------------------------------|-------------------------------------------|-----|
| Characters (incl. `ő ű á í`)   | `KEYEVENTF_UNICODE`, send UTF-16 directly | Bypasses host keyboard layout entirely. Hungarian just works. |
| Modifiers, F-keys, shortcuts   | `KEYEVENTF_SCANCODE` (+ VK filled in)     | Ctrl+C must be a real key event. Send both VK and scancode — DirectInput apps ignore VK-only. |

Soft keyboard → Unicode path. Separate sticky-toggle modifier bar (Ctrl/Alt/Shift/Tab/Esc/arrows)
→ scancode path. Sticky is non-negotiable; you cannot hold Ctrl on a touchscreen.

**Touch → mouse:**
- Trackpad mode (relative, drag to move, tap to click) — **default**
- Direct mode (tap teleports cursor) — toggle
- Two-finger scroll → `WHEEL`; long-press → right click; pinch → client viewport zoom only, never host

**Local cursor prediction:** draw and move the cursor client-side immediately, reconcile when the
host frame arrives. Without this every movement feels laggy by the full RTT even on a fast stream.

**Transport:** input on a separate UDP channel (ENet), not the video connection. On lossy LTE,
TCP head-of-line blocking means one dropped video packet stalls your keystrokes.

---

## 5. Known traps

| Trap | Symptom | Fix |
|------|---------|-----|
| PC sleeps | Tailscale can't reach it, nothing wakes it | Disable standby; or Raspberry Pi on the tailnet as a WoL relay |
| UAC / secure desktop | Prompts visible on stream but unclickable (UIPI) | Host must run as a service |
| Session 0 isolation | `SendInput` goes nowhere | Service spawns a helper in the active session via `CreateProcessAsUser` |
| Tailscale starts after login | Reboot = locked out | Set service start type accordingly |
| Backgrounded video stream | Data cap gone | Tear the pipeline fully down on tier exit — never pause |
| RDP console lock | Anyone at home sees a lock screen | Expected behaviour; use Sunshine if it matters |

---

## 6. Licensing

- OliveTin — AGPL-3.0 (matters if you ever fork it closed)
- ntfy — Apache 2.0 / GPLv2 dual
- RustDesk, Sunshine, Moonlight — GPL family
- Tailscale clients — BSD; Headscale — BSD

If this stays personal, none of it matters. If it ever ships, the AGPL boundary around OliveTin is
the one to keep clear of — which is another argument for writing your own agent in Phase 1.

---

## 7. Open questions for after Phase 0

- Which actions did you actually want buttons for? (Real list, not imagined.)
- Did Glance turn out to be the tier you use most? If yes, invest there, not in Tier 3.
- Is Tier 2 needed at all, or does Termius already cover it well enough?
- Multi-machine, or is one host enough forever?
