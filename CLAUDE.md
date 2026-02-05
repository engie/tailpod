# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

Tailpod: infrastructure-as-code for deploying rootless Podman containers on Fedora CoreOS, with each container joining a Tailscale network via ts4nsnet. Containers get a Tailscale identity, MagicDNS resolution, and are internet-isolated by default.

## Build & Run

**Prerequisites:** `brew install butane jq vfkit`

```bash
./build.sh          # Transpile tailpod.bu + secrets.bu → tailpod.ign
./boot.sh           # Launch FCOS VM (resets disk from .orig, clears EFI state)
./vm-ssh.sh         # SSH into running VM as core (resolves IP from DHCP leases)
./vm-ssh.sh 'bash -s' < VERIFY.sh   # Run post-boot verification
```

## Build Pipeline

```
tailpod.bu ──┐
             ├─→ butane --strict ─→ jq merge ─→ tailpod.ign ─→ vfkit --ignition
secrets.bu ──┘
```

- `build.sh` transpiles both `.bu` files independently, then merges their JSON (files arrays + passwd.users) with jq
- `mint-ts-authkey.sh` is inlined into the ignition via `contents.local:` in tailpod.bu
- `secrets.bu` is gitignored; create from `secrets.bu.example`

## Architecture

**On the VM (provisioned by Ignition at first boot):**
- `enable-linger-core.service` — allows rootless user services to start without a login session
- Quadlet files in `~core/.config/containers/systemd/` generate systemd user units
- Each container's `ExecStartPre` mints an ephemeral Tailscale auth key via OAuth, then ts4nsnet uses it to join the tailnet as the container's network namespace

**Key constraint:** Ignition only runs on first boot. The disk image must be fresh (boot.sh handles this by copying from `.raw.orig`).

## Common Pitfalls

- **Directory ownership:** All directories under `~core/.config/` must be owned by `core:core` in the butane config. Ignition creates intermediate directories as root, and Podman refuses to run if its config path has root-owned parents.
- **OAuth tag scope:** The tag in `-t tag:...` passed to `mint-ts-authkey.sh` must match what the Tailscale OAuth client is authorized for. A mismatch gives HTTP 400 from the Tailscale API.
- **No jq/python on FCOS:** `mint-ts-authkey.sh` parses JSON with sed. Keep it that way — FCOS is minimal.

## Secrets

`secrets.bu` contains Tailscale OAuth credentials and SSH keys. It's merged at build time and gitignored. The OAuth client ID/secret are used to mint short-lived ephemeral auth keys at container start — they are not the auth keys themselves.
