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
brew install butane
```

For local VM testing, also install `vfkit`. You also need Go 1.22+ (the build tool is a Go program).

## Setup

1. Copy the config template and fill in your values:

   ```
   cp site.env.example site.env
   ```

   You need to provide:
   - An SSH public key for the `core` user
   - Tailscale OAuth client ID and secret (for minting ephemeral auth keys)
   - Your tailnet domain (used in the `--dns-search` flag of the Tailscale transform)
   - Git URL and branch for your container definitions repo
   - SMB storage credentials (if using Litestream replication)

2. Place your SSH deploy key:

   ```
   cp /path/to/your/key deploy_key
   ```

3. Build the Ignition manifest:

   ```
   ./build.sh
   ```

   This runs a Go build tool (`cmd/build/`) that safely parses `site.env`, substitutes only allowlisted `${VAR}` patterns into `.bu` files, runs `butane --strict`, and optionally merges a `server.bu` overlay.

## Build pipeline

```
site.env ─→ cmd/build (Go) ─→ substitute ${VAR} ─→ butane --strict ─→ tailpod.ign
tailpod.bu ─┘                                                           ↑
server.bu (optional) ─→ same pipeline ─→ JSON merge ────────────────────┘
```

`site.env` contains site-specific values (credentials, tailnet domain, SSH pubkey). `tailpod.bu` contains all configuration with `${VAR}` placeholders. The Go build tool (`cmd/build/`) parses `site.env` as plain KEY=VALUE (no shell evaluation), substitutes only the 10 allowlisted variables, and pipes the result through `butane`. If `server.bu` exists, it's processed the same way and merged into the base ignition. Both `site.env` and `server.bu` are gitignored.

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
- **Duplicate users in Ignition** cause silent failure (VM boots but never reaches a shell). The build tool deduplicates by merging entries with the same name.
- **Directory ownership** under `~<user>/.config/` must belong to that user, not root. Podman refuses to start otherwise.
- **Container users must be non-system** so that `useradd` auto-allocates subuid/subgid ranges for rootless Podman.
- **OAuth tag scope** must match the `-tag` passed to tailmint, or the Tailscale API returns HTTP 400.
- **site.env is required.** All site-specific values (credentials, tailnet domain) are substituted from this file at build time.

## Related projects

- [quadsync](https://github.com/engie/quadsync) -- Git-to-Quadlet deployer with INI transforms
- [ts4nsnet](https://github.com/engie/ts4nsnet) -- Tailscale networking for rootless containers
- [tailmint](https://github.com/engie/tailmint) -- Tailscale auth key minter via OAuth
