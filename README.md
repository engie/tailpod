# tailpod

Infrastructure-as-code for deploying rootless Podman containers on Fedora CoreOS, with each container joining a Tailscale network. Container definitions live in a [separate git repo](https://github.com/engie/containers) and are synced to the host every two minutes by [quadsync](https://github.com/engie/quadsync), which applies directory-based INI transforms before deploying.

## How it works

tailpod is a pair of [Butane](https://coreos.github.io/butane/) configs that get transpiled into an [Ignition](https://coreos.github.io/ignition/) manifest. On first boot, the manifest provisions a Fedora CoreOS VM with everything needed to run the platform:

- Downloads three Go binaries from GitHub Releases (with SHA256 verification)
- Creates systemd units for periodic git sync
- Deploys config files, transforms, and credentials

Once running, the system operates autonomously:

```
quadsync-sync.timer (every 2min)
  └── quadsync sync
        ├── git pull container definitions
        ├── apply directory-based transforms (e.g. Tailscale networking)
        ├── create per-container Linux user
        └── write Quadlet file → systemd starts container
              ├── ExecStartPre: tailmint mints ephemeral auth key
              └── Podman runs with ts4nsnet as network bridge
                    └── container joins tailnet
```

## Prerequisites

```
brew install butane jq
```

For local VM testing, also install `vfkit`.

## Setup

1. Copy the secrets template and fill in your credentials:

   ```
   cp secrets.bu.example secrets.bu
   ```

   You need to provide:
   - An SSH public key for the `core` user
   - Tailscale OAuth client ID and secret (for minting ephemeral auth keys)
   - An SSH deploy key for your container definitions repo
   - Your tailnet domain (used in the `--dns-search` flag of the Tailscale transform)

2. Build the Ignition manifest:

   ```
   ./build.sh
   ```

   This transpiles both `.bu` files with `butane --strict`, then merges their JSON output with jq (deduplicating users by name to avoid silent Ignition failures).

## Build pipeline

```
tailpod.bu ─┐
             ├── butane --strict ──→ jq merge ──→ tailpod.ign
secrets.bu ─┘
```

`tailpod.bu` contains all non-secret configuration: directory layout, binary downloads, quadsync config, sudoers rules, and systemd units. `secrets.bu` contains credentials and the Tailscale transform (which includes the tailnet domain). Both are merged at build time; only `secrets.bu` is gitignored.

## What gets provisioned

### Binaries (downloaded at first boot)

| Binary | Purpose |
|--------|---------|
| [quadsync](https://github.com/engie/quadsync) | Git-sync deployer with directory-based INI transforms |
| [tailmint](https://github.com/engie/tailmint) | Mints short-lived Tailscale auth keys via OAuth |
| [ts4nsnet](https://github.com/engie/ts4nsnet) | slirp4netns replacement that bridges container traffic onto a tailnet |

### Config files

| File | Purpose |
|------|---------|
| `/etc/quadsync/config.env` | Git URL, branch, transform dir, user group |
| `/etc/quadsync/transforms/tailscale.container` | INI transform for Tailscale networking |
| `/etc/quadsync/deploy-key` | SSH key for pulling container definitions |
| `/etc/tailscale/oauth.env` | OAuth credentials for auth key minting |
| `/etc/sudoers.d/tailmint` | Allows container users to run tailmint as root |

### systemd units

| Unit | Purpose |
|------|---------|
| `enable-linger-core.service` | Enables lingering so rootless user services start at boot |
| `quadsync-sync.service` | Runs `quadsync sync` (oneshot) |
| `quadsync-sync.timer` | Triggers sync 30s after boot, then every 2min |

## Adding a container

Add a `.container` file to the `tailscale/` directory in your [container definitions repo](https://github.com/engie/containers):

```ini
# tailscale/my-app.container
[Unit]
Description=my-app tailpod

[Container]
Image=docker.io/library/nginx:latest
ContainerName=my-app
```

The Tailscale transform handles networking, auth key minting, and service lifecycle automatically. Within two minutes, the container will be running on your tailnet.

## Transform system

Transforms are `.container` INI files in `/etc/quadsync/transforms/`. The repo directory name maps to the transform filename:

- `*.container` (repo root) -- deployed as-is
- `tailscale/*.container` -- merged with `tailscale.container` transform

Merge rules:
- `Key=Value` -- sets a default (spec takes precedence)
- `+Key=Value` -- prepends before spec values (for multi-value keys like `ExecStartPre`)

## Pitfalls

- **Ignition is first-boot only.** The VM disk must be fresh. `boot.sh` handles this by copying from `.raw.orig`.
- **Duplicate users in Ignition** cause silent failure (VM boots but never reaches a shell). `build.sh` deduplicates with `group_by(.name)[] | add`.
- **Directory ownership** under `~<user>/.config/` must belong to that user, not root. Podman refuses to start otherwise.
- **Container users must be non-system** so that `useradd` auto-allocates subuid/subgid ranges for rootless Podman.
- **OAuth tag scope** must match the `-tag` passed to tailmint, or the Tailscale API returns HTTP 400.
- **secrets.bu is required.** The Tailscale transform lives there (it contains the tailnet domain), not in `tailpod.bu`.

## Related projects

- [quadsync](https://github.com/engie/quadsync) -- Git-to-Quadlet deployer with INI transforms
- [ts4nsnet](https://github.com/engie/ts4nsnet) -- Tailscale networking for rootless containers
- [tailmint](https://github.com/engie/tailmint) -- Tailscale auth key minter via OAuth
