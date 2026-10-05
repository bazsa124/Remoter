#!/bin/sh
# Remoter - install or update the hub on a Linux host. Run as root.
#
#   sudo sh install-hub.sh <staging-dir>
#
# <staging-dir> holds remoter-hub (linux binary) and optionally web/ - which is
# what deploy-hub.ps1 uploads. Idempotent: safe to re-run for every update.
set -eu

STAGE=${1:?usage: install-hub.sh <staging-dir>}
OPT=/opt/remoter
STATE=/var/lib/remoter
UNIT=/etc/systemd/system/remoter-hub.service

say() { printf '  [ok]   %s\n' "$*"; }

# A dedicated user, so the hub holds no more privilege than it needs. It owns
# the node tokens in state.json; nobody else on the box should be able to read
# them.
if ! id remoter >/dev/null 2>&1; then
    useradd --system --no-create-home --home-dir "$STATE" --shell /usr/sbin/nologin remoter
    say "created system user remoter"
fi

install -d -o root -g root -m 0755 "$OPT"
install -d -o remoter -g remoter -m 0700 "$STATE"

# Binary and client are root-owned: the service must not be able to rewrite
# its own code.
install -o root -g root -m 0755 "$STAGE/remoter-hub" "$OPT/remoter-hub.new"
mv -f "$OPT/remoter-hub.new" "$OPT/remoter-hub"
ln -sf "$OPT/remoter-hub" /usr/local/bin/remoter-hub
say "installed $OPT/remoter-hub"

if [ -d "$STAGE/web" ]; then
    rm -rf "$OPT/web.new"
    cp -r "$STAGE/web" "$OPT/web.new"
    # Keep the previous build's hashed assets: a page opened before this deploy
    # still asks for them by name. They are pruned after 30 days.
    if [ -d "$OPT/web/_app/immutable" ]; then
        mkdir -p "$OPT/web.new/_app/immutable"
        cp -rn "$OPT/web/_app/immutable/." "$OPT/web.new/_app/immutable/"
        find "$OPT/web.new/_app/immutable" -type f -mtime +30 -delete
    fi
    # Downloads published next to the client (APK, node installer) are not part
    # of the web build; carry them across rather than deleting them.
    if [ -d "$OPT/web/dl" ] && [ ! -d "$OPT/web.new/dl" ]; then
        cp -r "$OPT/web/dl" "$OPT/web.new/dl"
    fi
    chown -R root:root "$OPT/web.new"
    chmod -R a+rX,go-w "$OPT/web.new"
    rm -rf "$OPT/web.old"
    [ -d "$OPT/web" ] && mv "$OPT/web" "$OPT/web.old"
    mv "$OPT/web.new" "$OPT/web"
    rm -rf "$OPT/web.old"
    say "installed client to $OPT/web"
fi
if [ -d "$STAGE/dl" ]; then
    install -d -o root -g root -m 0755 "$OPT/web/dl"
    cp -f "$STAGE"/dl/* "$OPT/web/dl/"
    chmod -R a+rX,go-w "$OPT/web/dl"
    say "published downloads: $(ls "$OPT/web/dl" | tr '\n' ' ')"
fi

if [ -f "$STAGE/hub.json" ] && [ ! -f "$STATE/hub.json" ]; then
    install -o remoter -g remoter -m 0600 "$STAGE/hub.json" "$STATE/hub.json"
    say "seeded $STATE/hub.json"
fi

# Fetching TLS certificates from tailscaled is root-only unless the user is
# named here. Restarting tailscaled drops the tailnet for a few seconds - so
# only touch it when the setting is actually missing.
DEFAULTS=/etc/default/tailscaled
if ! grep -q '^TS_PERMIT_CERT_UID=remoter$' "$DEFAULTS" 2>/dev/null; then
    sed -i '/^TS_PERMIT_CERT_UID=/d' "$DEFAULTS" 2>/dev/null || true
    echo 'TS_PERMIT_CERT_UID=remoter' >> "$DEFAULTS"
    systemctl restart tailscaled
    say "allowed remoter to fetch TLS certificates (tailscaled restarted)"
    # Wait for the tailnet to come back before the hub tries to bind to it.
    for _ in $(seq 1 30); do
        tailscale ip -4 >/dev/null 2>&1 && break
        sleep 1
    done
fi

cat > "$UNIT" <<'EOF'
[Unit]
Description=Remoter hub (jump server)
Documentation=file:///opt/remoter
After=network-online.target tailscaled.service
Wants=network-online.target tailscaled.service

[Service]
User=remoter
Group=remoter
ExecStart=/opt/remoter/remoter-hub -dir /var/lib/remoter -web /opt/remoter/web
Restart=always
RestartSec=3

# Port 443 without root.
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes

# Confinement: the hub needs its state directory, tailscaled's socket and the
# network - nothing else.
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
LockPersonality=yes
ReadWritePaths=/var/lib/remoter
RuntimeDirectory=remoter
RuntimeDirectoryMode=0750

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable remoter-hub >/dev/null 2>&1
systemctl restart remoter-hub
say "remoter-hub (re)started"

sleep 2
if systemctl is-active --quiet remoter-hub; then
    say "running"
else
    echo "  [fail] remoter-hub is not running:" >&2
    journalctl -u remoter-hub -n 30 --no-pager >&2
    exit 1
fi
