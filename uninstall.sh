#!/bin/bash
# Removes the LaunchAgent. Does not touch the captured history;
# delete ~/.tunnel-inspector yourself if you want it gone.
set -euo pipefail
LABEL="io.github.fairytale5571.tunnel-inspector"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"

launchctl bootout "gui/$UID/$LABEL" 2>/dev/null || true
rm -f "$PLIST"
echo "Service removed."
echo "Remember to point your tunnel back at your app's port."
echo "History is still at ~/.tunnel-inspector/history.jsonl"
