# tailpod

Declarative container orchestration on [Tailscale](https://tailscale.com/). Define a container in a git repo, and within two minutes it's running on your tailnet with its own hostname — no port forwarding, no ingress controllers, no manual networking.

tailpod provisions a [Fedora CoreOS](https://fedoraproject.org/coreos/) VM via [Ignition](https://coreos.github.io/ignition/), then continuously syncs container definitions from git. Each container runs as a rootless [Podman](https://podman.io/) service under its own Linux user, with networking handled by a userspace Tailscale node.

## Architecture

```
You push a .container file to git
          │
          ▼
┌─────────────────────────────────────────────────────┐
│  quadsync-sync.timer (polls every 2 min)            │
│    └── quadsync sync                                │
│          ├── git pull container definitions          │
│          ├── merge with directory-based transforms   │
│          ├── create per-container Linux user         │
│          └── write Quadlet → systemd starts service  │
│                │                                     │
│                ▼                                     │
│          ExecStartPre: tailmint                      │
│            → mints ephemeral Tailscale auth key      │
│          ExecStartPre: storage-init                  │
│            → creates per-container SMB directories   │
│          Podman starts with ts4nsnet                 │
│            → enters container netns, creates TUN     │
│            → connects to tailnet via tsnet           │
│            → container is now reachable by hostname  │
└─────────────────────────────────────────────────────┘
```

### Components

| Component | Description |
|-----------|-------------|
| **tailpod** (this repo) | [Butane](https://coreos.github.io/butane/) config that provisions the VM at first boot |
| [quadsync](https://github.com/engie/quadsync) | Git-to-Quadlet deployer with INI transform system |
| [ts4nsnet](https://github.com/engie/ts4nsnet) | slirp4netns replacement — bridges container traffic onto a tailnet via [tsnet](https://pkg.go.dev/tailscale.com/tsnet) |
| [tailmint](https://github.com/engie/tailmint) | Mints short-lived Tailscale auth keys from OAuth credentials |

All three binaries are downloaded at first boot from GitHub Releases with SHA256 verification. quadsync and tailmint are stdlib-only; ts4nsnet depends only on the Tailscale SDK.

## Quick start

### Prerequisites

- [butane](https://coreos.github.io/butane/getting-started/) (`brew install butane`)
- Go 1.22+ (the build tool is a Go program)
- A [Tailscale OAuth client](https://tailscale.com/kb/1215/oauth-clients) with the `tag:tailpod` tag
- A git repo for your container definitions (see [Adding a container](#adding-a-container))
- An SSH deploy key with read access to that repo

### Setup

1. **Create `site.env`** from the template:

   ```bash
   cp site.env.example site.env
   ```

   Fill in all values:

   | Variable | Purpose |
   |----------|---------|
   | `SSH_PUBKEY` | SSH public key for the `core` user |
   | `TS_API_CLIENT_ID` | Tailscale OAuth client ID |
   | `TS_API_CLIENT_SECRET` | Tailscale OAuth client secret |
   | `TAILNET_DOMAIN` | Your tailnet domain (e.g. `example.ts.net`) |
   | `QUADSYNC_GIT_URL` | SSH URL of your container definitions repo |
   | `QUADSYNC_GIT_BRANCH` | Branch to track (e.g. `main`) |
   | `STORAGE_SMB_*` | SMB credentials for persistent storage |

2. **Place your deploy key:**

   ```bash
   cp /path/to/your/key deploy_key
   ```

3. **Build the Ignition manifest:**

   ```bash
   ./build.sh
   ```

4. **Boot a Fedora CoreOS instance** with `tailpod.ign` as the Ignition config.

The VM will download binaries, mount SMB storage, and start the sync timer. Containers from your git repo will be deployed within two minutes.

## Adding a container

Add a `.container` file under the `tailscale/` directory in your container definitions repo:

```ini
# tailscale/my-app.container
[Unit]
Description=my-app tailpod

[Container]
Image=docker.io/library/nginx:latest
ContainerName=my-app
```

Push it. Within two minutes, quadsync picks it up, creates a `my-app` Linux user, merges the Tailscale transform, and starts the service. The container joins your tailnet as `my-app` and is reachable via MagicDNS.

To remove a container, delete the file from git. quadsync will stop the service, remove the Quadlet, and delete the user on the next sync.

## Transform system

Transforms inject host-level configuration into container specs without modifying the specs themselves. They live in `/etc/quadsync/transforms/` and are matched by repo directory name:

| Repo path | Transform applied |
|-----------|------------------|
| `*.container` (root) | `_base.container` only |
| `tailscale/*.container` | `_base.container` + `tailscale.container` |
| `foo/*.container` | `_base.container` + `foo.container` |

**Merge rules:**
- `Key=Value` — sets a default (the spec takes precedence if it defines the same key)
- `+Key=Value` — prepends before the spec's values (for multi-value keys like `ExecStartPre`)

### Built-in transforms

**`_base.container`** — Applied to all containers. Mounts a per-container named volume at `/data` and runs `storage-init` to create the container's directory on the SMB share.

**`tailscale.container`** — Adds Tailscale networking. Sets up `ts4nsnet` as the network command, configures Tailscale DNS (`100.100.100.100` + your tailnet domain as search suffix), and prepends `ExecStartPre` steps that mint a fresh ephemeral auth key via `tailmint`.

### Companion templates

Transforms can include companion files. The `_base` transform ships with:

- **`_base-data.volume`** — A named Podman volume for each container, mounted at `/data`.
- **`_base-litestream.container`** — An optional Litestream sidecar that replicates `/data/db.sqlite` to the SMB share. Deployed automatically if the container definitions repo includes a matching file.

## Persistent storage

tailpod mounts an SMB share at `/var/mnt/storage`. Each container gets:

- A **named Podman volume** (`<name>-data`) mounted at `/data` inside the container
- A **directory on the SMB share** (`/var/mnt/storage/<name>/db_backup`) for off-host backups

The Litestream sidecar, if enabled, continuously replicates SQLite databases from the volume to the SMB backup directory.

## Build pipeline

```
site.env ─→ cmd/build (Go) ─→ substitute ${VAR} ─→ butane --strict ─→ tailpod.ign
tailpod.bu ─┘                                                           ↑
server.bu (optional) ─→ same pipeline ─→ JSON merge ────────────────────┘
```

The build tool (`cmd/build/`) parses `site.env` as plain `KEY=VALUE`, substitutes only the 10 allowlisted variables into `.bu` files using strict `${VAR}` matching, and pipes the result through `butane --strict`. Unknown `${...}` patterns are left intact.

If `server.bu` exists (gitignored), it's processed the same way and merged into the base Ignition — use it for per-server customization like additional mounts or scheduled tasks. See `server.bu.example`.

## What gets provisioned

### Binaries

| Binary | Version | Purpose |
|--------|---------|---------|
| [quadsync](https://github.com/engie/quadsync) | v0.4 | Git-sync deployer with INI transforms |
| [ts4nsnet](https://github.com/engie/ts4nsnet) | v0.2 | Userspace Tailscale networking for containers |
| [tailmint](https://github.com/engie/tailmint) | v0.3 | Ephemeral auth key minting via OAuth |

### Config files

| Path | Purpose |
|------|---------|
| `/etc/quadsync/config.env` | Git URL, branch, transform dir, user group |
| `/etc/quadsync/transforms/` | INI transforms (tailscale, \_base, companions) |
| `/etc/quadsync/deploy-key` | SSH key for the container definitions repo |
| `/etc/tailscale/oauth.env` | OAuth credentials for auth key minting |
| `/etc/samba/storage-credentials` | SMB credentials for the storage mount |
| `/etc/sudoers.d/tailmint` | Constrained sudo for container users to mint keys |
| `/etc/sudoers.d/storage-init` | Constrained sudo for container users to init storage |

### systemd units

| Unit | Purpose |
|------|---------|
| `quadsync-sync.timer` | Polls for container changes every 2 minutes |
| `quadsync-sync.service` | Runs `quadsync sync` (oneshot) |
| `var-mnt-storage.mount` | Mounts SMB share at `/var/mnt/storage` |

## Security model

- **Ephemeral auth keys** — OAuth credentials stay on the host. Each container startup mints a fresh, single-use, short-lived Tailscale auth key. No key reuse.
- **Per-container isolation** — Each container runs as its own non-root Linux user with rootless Podman. Users are auto-created with dedicated subuid/subgid ranges.
- **Constrained sudo** — Container users can only run `tailmint` and `storage-init` with specific argument patterns. The sudoers rules use glob matching to prevent argument injection.
- **Allowlisted substitution** — The build tool only substitutes 10 named variables. Shell evaluation is never used.
- **Credential separation** — `site.env`, `deploy_key`, and `tailpod.ign` are all gitignored. The Ignition manifest is written with mode 0600.

## Pitfalls

- **Ignition is first-boot only.** The VM disk must be fresh — Ignition does not re-run on existing installs.
- **OAuth tag scope** must match the `-tag` passed to tailmint (`tag:tailpod` by default), or the Tailscale API returns HTTP 400.
- **Container users must be non-system** for `useradd` to auto-allocate subuid/subgid ranges. Without these, rootless Podman fails.
- **Directory ownership** under `~<user>/.config/` must belong to that user. Podman refuses to start otherwise. quadsync handles this, but be aware if debugging.
- **`site.env` is required.** The build fails if any of the 10 variables are missing.

## License

MIT
