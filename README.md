# Remoter

Remote access to your own computers and phone, entirely inside your
[Tailscale](https://tailscale.com) tailnet.

One always-on Linux machine runs the **hub**. Every device you want to reach runs a
small **node** service that answers the hub and nothing else. You control everything
from a browser or the Android app, and every session is relayed through the hub,
so nothing is ever exposed to the internet or even to the rest of the tailnet.

```
 controllers                    hub                        targets
 browser / Android app ──HTTPS──▶ remoter-hub ──relay──▶ remoter-node (Windows)
                                   who-is auth            remoter-node (the hub machine)
                                   approval + PIN         Android app (phone as target)
                                   audit, push
                    ═══════════ all over Tailscale ═══════════
```

## What you can do

Four tiers, from lightest to heaviest:

| Tier | What it does | Windows | Hub machine (Linux) | Android phone |
|---|---|---|---|---|
| **Actions** | Run a whitelisted command; the result is kept as a job | ✓ | ✓ | – |
| **Glance** | One screenshot, on demand | ✓ incl. lock screen | – | ✓ Android 11+ |
| **Live** | Streamed screen with mouse, keyboard and touch | ✓ incl. lock screen | – | ✓ |
| **Console** | Interactive shell | ✓ PowerShell, as the signed-in user | ✓ login shell | – |

Also:

* **Windows works unattended:** after a reboot, at the lock screen, before anyone signs in.
* **Arming** keeps a laptop awake (lid closed, on battery) while you are away. It disarms itself
  on a timer, on low battery, or when someone uses the keyboard or mouse locally.
* **Push notifications** through [ntfy](https://ntfy.sh): job finished, session started, armed
  device went offline, new device waiting for approval.
* **Phone as a target** has two modes: unattended (an accessibility service, about 1 frame/s)
  and smooth (screen capture, which Android asks you to allow on the phone).
* A **Wi-Fi safety net** on Windows nudges adapters that are slow to auto-connect, during the
  first minutes after boot and while armed.

## Security model

* **Nothing listens beyond the tailnet.** The hub binds its tailnet address only. Nodes accept
  connections from the hub's address alone, checked in code and, on Windows, by a firewall rule.
* **Identity comes from Tailscale.** There are no accounts or passwords. The hub asks tailscaled
  who is connecting, and unknown devices stay *pending* until you approve them.
* **A PIN guards Glance, Live, Console and approvals.** It is hashed with argon2id, stays unlocked
  for 5 minutes after use, and locks out after repeated wrong guesses. The Android app can unlock
  it with your fingerprint.
* **The hub and nodes share per-node tokens**, and the hub refuses cross-origin and DNS-rebinding
  requests.
* **Every session is visible on the target:** a banner and tray icon on Windows, a notification
  on Android.
* **Everything is audited:** sessions, approvals, PIN attempts and action runs.
* **Actions are a whitelist.** A node runs nothing that is not in its `actions.yaml`.

The hub is a hard dependency by design: when it is down, nothing is reachable.

## Repository layout

| Path | What |
|---|---|
| `agent/` | Go module: `cmd/remoter-hub` and `cmd/remoter-node`, with everything under `internal/` |
| `web/` | SvelteKit web client (static SPA, served by the hub) |
| `android/` | Android app (Kotlin, Compose): controller, and optionally a target |
| `deploy/` | Build, deploy and install scripts |
| `HUB-ARCHITECTURE.md` | Design, decisions and the build log per phase |
| `IMPLEMENTATION-PLAN.md`, `tiered-remote-control-spec.md` | The earlier single-machine design this grew out of |

## Requirements

* **Tailscale** on every device, with **MagicDNS** and **HTTPS Certificates** enabled in the
  admin console (DNS page). The hub's certificate comes from tailscaled.
* **Hub machine:** Linux with systemd, tailscaled and `python3`, reachable over SSH by a user
  with sudo.
* **Build machine:** Windows with PowerShell, Go 1.25+, Node.js with pnpm, and the OpenSSH
  client. For the APK: Android Studio (its bundled JDK and the Android SDK).
* **Targets:** Windows 11, or the hub machine itself. Android 10+ for the app; the phone's
  unattended target mode needs Android 11+.

## Getting started

**1. Build.** On the build machine:

```powershell
.\deploy\build.ps1            # node (Windows), hub (Linux), web client -> .\dist
.\deploy\build.ps1 -Android   # also the APK
```

To bake your hub's address into the APK, put it in `android/local.properties`, which is
git-ignored. Without it, the app asks for the address on first start.

```properties
remoter.hub=https://<hub>.<tailnet>.ts.net
```

**2. Deploy the hub.** Add an SSH host alias `hub` for the hub machine to `~/.ssh/config`, then:

```powershell
.\deploy\deploy-hub.ps1       # or -SshHost <alias>; -SkipNode to not make the hub a target
```

This installs `remoter-hub` as a systemd service (user `remoter`, HTTPS on the tailnet
address), publishes the web client and downloads, and installs a node on the hub machine
itself (Console and Actions, as your user). It asks for the sudo password once.

**3. Turn on pushes (optional).** Set `ntfyTopic` in `/var/lib/remoter/hub.json` to a long,
random topic name, since anyone who knows it can read the pushes. Then run
`sudo systemctl restart remoter-hub`.

**4. Approve yourself.** Open `https://<hub>.<tailnet>.ts.net`. Your device shows as waiting.
Approve it on the hub machine:

```sh
sudo remoter-hub list
sudo remoter-hub approve <device-name>
```

Then set a PIN on the **Access** page. Later devices can be approved from there.

**5. Add a Windows PC.** The Access page shows the exact command. In PowerShell on the PC:

```powershell
irm https://<hub>.<tailnet>.ts.net/dl/install-node.ps1 -OutFile $env:TEMP\install-node.ps1
powershell -ExecutionPolicy Bypass -File $env:TEMP\install-node.ps1 -Hub https://<hub>.<tailnet>.ts.net
```

It asks for administrator rights once, installs the `RemoterNode` service, enrols with the hub
and puts Tailscale in unattended mode. Approve the PC on the Access page. To update the node
later, run `install-node.ps1` again without `-Hub`; `-Uninstall` removes it.

**6. Add the phone.** Download the APK from the Access page (`/dl/remoter.apk`) and install it.
The release build is signed with the debug key and is meant for sideloading. Approve the phone.
To make it a target too, tap **Make this phone controllable** in the app and approve it again,
under "wants to be controllable".

## Configuration

* **Hub:** `/var/lib/remoter/hub.json` holds the port, node port, ntfy server and topic, and
  socket paths. It is written with defaults on first start. Approvals, tokens and the PIN hash
  live in `state.json`, and the audit log in `audit.log`.
* **Windows node:** state lives in `%ProgramData%\Remoter`, writable by SYSTEM and administrators
  only. Its token is readable by them alone.
* **Linux node:** `~/.config/remoter/` of the user it runs as.
* **Actions:** `actions.yaml` next to the node's config.
  [`agent/actions.example.yaml`](agent/actions.example.yaml) and
  [`deploy/actions-hub.yaml`](deploy/actions-hub.yaml) are examples. Restart the node after
  editing.

Admin commands on the hub machine:

```sh
sudo remoter-hub list | approve <name> | revoke <name> | set-pin | clear-pin
```

## Development

```sh
cd agent && go test ./...            # Go tests
cd web && pnpm install && pnpm check
```

To run the web client against a real hub, name the hub in `web/.env.local` (git-ignored) and
start the dev server. It proxies the API to the hub:

```sh
echo REMOTER_HUB=https://<hub>.<tailnet>.ts.net > web/.env.local
cd web && pnpm dev
```

`.\deploy\build.ps1 -CrossCheck` also vets the Go code for every target platform.
