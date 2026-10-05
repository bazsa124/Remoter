#!/bin/sh
# Remoter - install or update the node on a Linux machine. Run as root.
#
#   sudo sh install-node.sh <staging-dir> <user> [hub-url]
#
# <staging-dir> holds remoter-node (linux binary) and optionally actions.yaml,
# which seeds the user's action list on first install. The node runs as <user>:
# Console is that user's shell and Actions run with that user's rights - never
# root's. Idempotent: re-run it to update.
#
# On the hub's own machine the node listens on loopback only; elsewhere
# it binds the tailnet address and accepts the hub alone.
set -eu

STAGE=${1:?usage: install-node.sh <staging-dir> <user> [hub-url]}
RUNAS=${2:?usage: install-node.sh <staging-dir> <user> [hub-url]}
HUB=${3:-}
OPT=/opt/remoter
UNIT=/etc/systemd/system/remoter-node.service

say() { printf '  [ok]   %s\n' "$*"; }

HOME_DIR=$(getent passwd "$RUNAS" | cut -d: -f6)
[ -n "$HOME_DIR" ] || { echo "no such user: $RUNAS" >&2; exit 1; }
CFG="$HOME_DIR/.config/remoter"

install -d -o root -g root -m 0755 "$OPT"
install -o root -g root -m 0755 "$STAGE/remoter-node" "$OPT/remoter-node.new"
mv -f "$OPT/remoter-node.new" "$OPT/remoter-node"
say "installed $OPT/remoter-node"

# Is the hub this very machine? Ask tailscaled for our own MagicDNS name.
SELF_NAME=$(tailscale status --json 2>/dev/null |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["Self"]["DNSName"].rstrip("."))' 2>/dev/null || true)
SELF_IP=$(tailscale ip -4 2>/dev/null | head -n1)
# No hub given: this must be the hub's own machine, whose node answers
# to the hub running right here.
[ -n "$HUB" ] || HUB="https://$SELF_NAME"
HUBHOST=$(printf '%s' "$HUB" | sed -e 's#^https://##' -e 's#/.*$##')
LOCAL_HUB=0
[ "$SELF_NAME" = "$HUBHOST" ] && LOCAL_HUB=1

if [ "$LOCAL_HUB" = 1 ]; then
    LISTEN='127.0.0.1:8737'
    # This machine may not use Tailscale's DNS (e.g. it runs its own resolver), so its own
    # hub name might not resolve. Pin it to our tailnet address - the same
    # answer MagicDNS would give.
    if ! getent hosts "$HUBHOST" >/dev/null 2>&1; then
        printf '%s %s # Remoter: the hub runs on this machine\n' "$SELF_IP" "$HUBHOST" >> /etc/hosts
        say "pinned $HUBHOST to $SELF_IP in /etc/hosts"
    fi
else
    LISTEN='tailscale:8737'
fi

install -d -o "$RUNAS" -m 0700 "$HOME_DIR/.config" 2>/dev/null || true
install -d -o "$RUNAS" -m 0700 "$CFG"
if [ ! -f "$CFG/config.json" ]; then
    cat > "$CFG/config.json" <<EOF
{
  "listen": ["$LISTEN"],
  "actionsFile": "actions.yaml",
  "maxJobs": 100,
  "console": { "enabled": true }
}
EOF
    chown "$RUNAS" "$CFG/config.json"
    chmod 0600 "$CFG/config.json"
    say "wrote $CFG/config.json (listen $LISTEN)"
fi
if [ ! -f "$CFG/actions.yaml" ] && [ -f "$STAGE/actions.yaml" ]; then
    install -o "$RUNAS" -m 0600 "$STAGE/actions.yaml" "$CFG/actions.yaml"
    say "seeded $CFG/actions.yaml"
fi

# Enrol as the user, so the token and config it writes belong to them.
sudo -u "$RUNAS" -H "$OPT/remoter-node" enroll -hub "$HUB"

cat > "$UNIT" <<EOF
[Unit]
Description=Remoter node (this machine as a target)
After=network-online.target tailscaled.service
Wants=network-online.target

[Service]
User=$RUNAS
ExecStart=$OPT/remoter-node
Restart=always
RestartSec=3
# Deliberately no NoNewPrivileges: Console is a real login shell, and the
# user may need sudo in it.

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable remoter-node >/dev/null 2>&1
systemctl restart remoter-node
sleep 2
if systemctl is-active --quiet remoter-node; then
    say "remoter-node running as $RUNAS"
else
    echo "  [fail] remoter-node is not running:" >&2
    journalctl -u remoter-node -n 30 --no-pager >&2
    exit 1
fi
