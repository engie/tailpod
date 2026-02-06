# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

Tailpod: infrastructure-as-code for deploying rootless Podman containers on Fedora CoreOS, with each container joining a Tailscale network via ts4nsnet. Container definitions live in a separate git repo and are synced to the host by `quadlet-deploy`, which applies directory-based INI transforms (e.g. Tailscale networking) before deploying.

## Build & Run

**Prerequisites:** `brew install butane jq vfkit` and Go 1.22+

```bash
./build.sh          # Build Go binaries + transpile tailpod.bu + secrets.bu → tailpod.ign
./boot.sh           # Launch FCOS VM (resets disk from .orig, clears EFI state)
./vm-ssh.sh         # SSH into running VM as core (resolves IP from DHCP leases)
./vm-ssh.sh 'bash -s' < VERIFY.sh   # Run post-boot verification
```

## Build Pipeline

```
quadlet-deploy/ ──→ go build ──→ quadlet-deploy.bin ──┐
tailpod-mint-key/ → go build ──→ tailpod-mint-key.bin ─┤
tailpod.bu ────────────────────────────────────────────┼─→ butane --strict ─→ jq merge ─→ tailpod.ign
secrets.bu ────────────────────────────────────────────┘
```

- `build.sh` builds both Go binaries (linux/arm64, static), then transpiles both `.bu` files and merges their JSON with jq
- `secrets.bu` is gitignored; create from `secrets.bu.example`

## Architecture

**Two binaries:**
- `quadlet-deploy` — Generic git→quadlet deployer with directory-based INI transforms. Subcommands: `sync`, `check`, `augment`.
- `tailpod-mint-key` — Standalone Tailscale auth key minter via OAuth. Referenced by the tailscale transform, not by quadlet-deploy.

**On the VM (provisioned by Ignition at first boot):**
- `enable-linger-core.service` — allows rootless user services to start without a login session
- `quadlet-deploy-sync.timer` — polls git repo every 2min, deploys/removes containers
- Transform files in `/etc/quadlet-deploy/transforms/` are merged into specs from matching repo directories
- Each Tailscale container's `ExecStartPre` runs `sudo tailpod-mint-key` for a fresh ephemeral auth key

**Key constraint:** Ignition only runs on first boot. The disk image must be fresh (boot.sh handles this by copying from `.raw.orig`).

## Go Modules

Both modules use standard library only (no external dependencies, no CGO).

```bash
cd quadlet-deploy && go test ./...     # INI parser + merge logic tests
cd tailpod-mint-key && go test ./...   # OAuth flow + file write tests
```

## Transform System

Transform files are `.container` INI files in `/etc/quadlet-deploy/transforms/`. The deployer maps repo directory names to transform filenames:

- `*.container` (repo root) → deployed as-is, no transform
- `tailscale/*.container` → merged with `tailscale.container` transform
- `foo/*.container` → merged with `foo.container` transform

**Merge rules:**
- `Key=Value` — set only if the spec hasn't set this key (user takes precedence)
- `+Key=Value` — prepend before the spec's values (for multi-value keys like ExecStartPre)

## Container Definitions Repo

Container specs live in a separate git repo (`git@github.com:engie/containers.git`). To add a container on the tailnet, add a `.container` file under `tailscale/`:

```ini
# tailscale/my-app.container
[Unit]
Description=my-app tailpod

[Container]
Image=docker.io/library/nginx:latest
ContainerName=my-app
```

The tailscale transform handles all networking, auth key minting, and service lifecycle automatically.

## Common Pitfalls

- **Directory ownership:** All directories under `~<user>/.config/` must be owned by that user. Ignition and `os.MkdirAll` (in quadlet-deploy) create directories as root. Podman refuses to run if its config path has root-owned parents. `writeQuadlet` chowns `.config` after creating dirs.
- **Ignition duplicate users:** The jq merge in `build.sh` must merge `passwd.users` by name (`group_by(.name)[] | add`), not concatenate arrays. Duplicate user entries cause Ignition to fail silently — the VM boots but never reaches a shell.
- **systemd Environment= quoting:** Values with spaces must be quoted: `Environment="GIT_SSH_COMMAND=ssh -i /path -o Opt=val"`. Without quotes, systemd splits on spaces and only sets the first word.
- **Regular users, not system users:** Container users must be created without `--system` so that `useradd` auto-allocates subuid/subgid ranges. Without these, rootless Podman fails with "insufficient UIDs or GIDs available in user namespace".
- **User manager startup delay:** After `useradd` + `loginctl enable-linger`, the user's systemd instance takes time to start (can exceed 30s on first boot). `quadlet-deploy` waits for it, but if it times out the deploy retries on the next sync cycle.
- **cusers group membership:** Only container users should be in `cusers`. Adding `core` causes `quadlet-deploy` to treat it as a managed container and attempt to delete it during cleanup.
- **OAuth tag scope:** The tag in `-tag tag:...` passed to `tailpod-mint-key` must match what the Tailscale OAuth client is authorized for. A mismatch gives HTTP 400 from the Tailscale API.
- **Transform in secrets.bu:** The tailscale transform contains the tailnet domain in `--dns-search`. It's provisioned via `secrets.bu`, not `tailpod.bu`.

## Secrets

`secrets.bu` contains Tailscale OAuth credentials, SSH deploy key, and the tailscale transform (which includes the tailnet domain). It's merged at build time and gitignored. The OAuth client ID/secret are used to mint short-lived ephemeral auth keys at container start — they are not the auth keys themselves.

## Config Files on VM

| File | Purpose | Provisioned by |
|------|---------|---------------|
| `/etc/quadlet-deploy/config.env` | Git URL, branch, transform dir, group | `tailpod.bu` |
| `/etc/quadlet-deploy/transforms/tailscale.container` | ts4nsnet merge template | `secrets.bu` |
| `/etc/quadlet-deploy/deploy-key` | SSH deploy key (0600) | `secrets.bu` |
| `/etc/tailscale/oauth.env` | OAuth client ID/secret (0600) | `secrets.bu` |
| `/etc/sudoers.d/tailpod-mint-key` | NOPASSWD for cusers group | `tailpod.bu` |
