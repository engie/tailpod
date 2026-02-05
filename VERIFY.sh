#!/usr/bin/env bash
# Verification plan for tailpod: FCOS + rootless Podman + ts4nsnet
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
for f in /usr/local/bin/ts4nsnet /usr/local/bin/mint-ts-authkey.sh; do
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

# 3. Check credentials present
echo "3. Credentials"
if [[ -f ~/.config/tailscale/keymint.env ]]; then
  pass "keymint.env exists"
else
  fail "keymint.env not found"
fi
echo

# 4. Check containers.conf has dns search
echo "4. containers.conf"
if grep -q 'dns_searches' ~/.config/containers/containers.conf 2>/dev/null; then
  pass "dns_searches configured"
  grep 'dns_searches' ~/.config/containers/containers.conf
else
  fail "containers.conf missing or no dns_searches"
fi
echo

# 5. Check quadlet unit generated
echo "5. Quadlet"
if systemctl --user list-unit-files 2>/dev/null | grep -q nginx-demo; then
  pass "nginx-demo unit found"
else
  fail "nginx-demo unit not found"
fi
echo

# 6. Test auth key minting (optional — requires valid OAuth creds)
echo "6. Auth key minting (dry run)"
if mint-ts-authkey.sh -c ~/.config/tailscale/keymint.env -t tag:container-nginx-demo --print 2>/dev/null; then
  pass "Auth key minted successfully"
else
  fail "Auth key minting failed (check OAuth credentials)"
fi
echo

# 7. Start the container
echo "7. Start nginx-demo"
if systemctl --user start nginx-demo 2>/dev/null; then
  pass "nginx-demo started"
  sleep 5
else
  fail "nginx-demo failed to start"
fi
echo

# 8. Check it joined tailnet
echo "8. Tailnet membership"
if podman exec nginx-demo nslookup nginx-demo 100.100.100.100 &>/dev/null; then
  pass "nginx-demo resolvable via MagicDNS"
else
  fail "nginx-demo not resolvable via MagicDNS (may need more time)"
fi
echo

# 9. Verify DNS resolution inside container
echo "9. DNS search domain"
if podman exec nginx-demo cat /etc/resolv.conf 2>/dev/null | grep -q 'search'; then
  pass "search domain present in container resolv.conf"
  podman exec nginx-demo grep 'search' /etc/resolv.conf
else
  fail "no search domain in container resolv.conf"
fi
echo

# 10. Verify no internet access (expect failure)
echo "10. Internet isolation"
if podman exec nginx-demo curl -m5 https://google.com &>/dev/null; then
  fail "Container has internet access (expected isolation)"
else
  pass "No internet access (as expected)"
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
