#!/usr/bin/env bash
# Verification plan for tailpod: FCOS + rootless Podman + quadsync + ts4nsnet
# Run these commands after booting with the generated ignition.
#
# Usage: ssh into the FCOS host as core, or run remotely with:
#   ssh core@<host> 'bash -s' < VERIFY.sh

set -euo pipefail

pass() { printf '  ✓ %s\n' "$1"; }
fail() { printf '  ✗ %s\n' "$1"; FAILS=$((FAILS + 1)); }
FAILS=0

echo "=== Tailpod Verification ==="
echo

# 1. Check binaries deployed
echo "1. Binaries"
for f in /usr/local/bin/ts4nsnet /usr/local/bin/quadsync /usr/local/bin/tailmint; do
  if [[ -x "$f" ]]; then pass "$f exists and is executable"
  else fail "$f missing or not executable"; fi
done
echo

# 2. Check linger enabled
echo "2. Linger"
if loginctl show-user core --property=Linger 2>/dev/null | grep -q 'Linger=yes'; then
  pass "Linger=yes for core"
else
  fail "Linger not enabled for core"
fi
echo

# 3. Check OAuth credentials present
echo "3. Credentials"
if [[ -f /etc/tailscale/oauth.env ]]; then
  pass "oauth.env exists"
else
  fail "oauth.env not found"
fi
echo

# 4. Check quadsync config
echo "4. quadsync config"
if [[ -f /etc/quadsync/config.env ]]; then
  pass "config.env exists"
else
  fail "config.env not found"
fi
if [[ -d /etc/quadsync/transforms ]]; then
  pass "transforms directory exists"
else
  fail "transforms directory not found"
fi
echo

# 5. Check tailscale transform
echo "5. Tailscale transform"
if [[ -f /etc/quadsync/transforms/tailscale.container ]]; then
  pass "tailscale.container transform exists"
  if grep -q 'ts4nsnet' /etc/quadsync/transforms/tailscale.container 2>/dev/null; then
    pass "transform references ts4nsnet"
  else
    fail "transform does not reference ts4nsnet"
  fi
else
  fail "tailscale.container transform not found"
fi
echo

# 6. Check deploy key
echo "6. Deploy key"
if [[ -f /etc/quadsync/deploy-key ]]; then
  pass "deploy-key exists"
  perms=$(stat -c '%a' /etc/quadsync/deploy-key 2>/dev/null || stat -f '%Lp' /etc/quadsync/deploy-key)
  if [[ "$perms" == "600" ]]; then
    pass "deploy-key has mode 0600"
  else
    fail "deploy-key has mode $perms (expected 0600)"
  fi
else
  fail "deploy-key not found"
fi
echo

# 7. Check sudoers
echo "7. Sudoers"
if sudo test -f /etc/sudoers.d/tailmint; then
  pass "sudoers file exists"
else
  fail "sudoers file not found"
fi
echo

# 8. Check sync timer
echo "8. Sync timer"
if systemctl is-enabled quadsync-sync.timer &>/dev/null; then
  pass "quadsync-sync.timer is enabled"
else
  fail "quadsync-sync.timer not enabled"
fi
echo

# 9. Check cusers group
echo "9. cusers group"
if getent group cusers &>/dev/null; then
  pass "cusers group exists"
else
  fail "cusers group not found"
fi
if ! id -nG core 2>/dev/null | grep -qw cusers; then
  pass "core is not in cusers group (only container users should be)"
else
  fail "core should not be in cusers group"
fi
echo

# 10. Test quadsync check (if repo has been cloned)
echo "10. quadsync"
if sudo GIT_SSH_COMMAND='ssh -i /etc/quadsync/deploy-key -o StrictHostKeyChecking=accept-new' quadsync sync 2>&1 | head -5; then
  pass "quadsync sync ran (check output above)"
else
  fail "quadsync sync failed (may need deploy key or network)"
fi
echo

# Summary
echo "=== Done ==="
if [[ $FAILS -eq 0 ]]; then
  echo "All checks passed."
else
  echo "$FAILS check(s) failed."
  exit 1
fi
