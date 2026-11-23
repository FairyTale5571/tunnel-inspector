#!/bin/bash
# Builds tunnel-inspector and installs it as a LaunchAgent that starts at
# login and is restarted by launchd if it ever dies.
#
#   ./install.sh                     # defaults: 4010 -> 3000, UI on 4041
#   ./install.sh 4010 3000 4041      # proxy port, app port, UI port
set -euo pipefail

LABEL="io.github.fairytale5571.tunnel-inspector"
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"

PROXY_PORT="${1:-4010}"
APP_PORT="${2:-3000}"
UI_PORT="${3:-4041}"

command -v go >/dev/null || { echo "Go is required: brew install go" >&2; exit 1; }

echo "==> Building"
cd "$DIR"
go build -ldflags="-s -w" -o tunnel-inspector .

echo "==> Writing $PLIST"
mkdir -p "$HOME/Library/LaunchAgents"
sed -e "s|__LABEL__|$LABEL|g" \
    -e "s|__BINARY__|$DIR/tunnel-inspector|g" \
    -e "s|__LISTEN__|localhost:$PROXY_PORT|g" \
    -e "s|__UPSTREAM__|http://localhost:$APP_PORT|g" \
    -e "s|__UI__|localhost:$UI_PORT|g" \
    -e "s|__WORKDIR__|$DIR|g" \
    -e "s|__HOME__|$HOME|g" \
    launchagent.plist.template > "$PLIST"
plutil -lint "$PLIST" >/dev/null

echo "==> (Re)starting the service"
launchctl bootout "gui/$UID/$LABEL" 2>/dev/null || true
launchctl bootstrap "gui/$UID" "$PLIST"

echo "==> Waiting for the UI to answer"
for _ in $(seq 1 40); do
  curl -sf -o /dev/null "http://127.0.0.1:$UI_PORT/" && break
  sleep 0.25
done

cat <<TXT

Done.

  Inspector UI   http://127.0.0.1:$UI_PORT
  Proxy listens  localhost:$PROXY_PORT  (IPv4 + IPv6 loopback)
  Forwards to    http://localhost:$APP_PORT

Last step, once: point your tunnel at the proxy instead of your app.
Cloudflare Zero Trust -> Networks -> Tunnels -> your tunnel -> Public hostname,
change the service from http://localhost:$APP_PORT to http://localhost:$PROXY_PORT.

Your app keeps running on port $APP_PORT. Nothing else to change, ever.
TXT
