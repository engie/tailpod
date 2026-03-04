# tailpod

Declarative container orchestration on [Tailscale](https://tailscale.com/). Define a container in a git repo, and within two minutes it's running on your tailnet with its own hostname — no port forwarding, no ingress controllers, no manual networking.

tailpod provisions a [Fedora CoreOS](https://fedoraproject.org/coreos/) VM via [Ignition](https://coreos.github.io/ignition/), then continuously syncs container definitions from git. Each container runs as a rootless [Podman](https://podman.io/) service under its own Linux user. Optional overlays add Tailscale networking (`tailscale.bu`) and persistent SMB storage (`server.bu`).

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
│          ExecStartPre: tailmint (tailscale.bu)       │
│            → mints ephemeral Tailscale auth key      │
│          ExecStartPre: storage-init (server.bu)      │
│            → creates per-container SMB directories   │
│          Podman starts with ts4nsnet (tailscale.bu)   │
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

quadsync is downloaded by the base config. ts4nsnet and tailmint are added by `tailscale.bu`. All binaries are fetched at first boot from GitHub Releases with SHA256 verification. quadsync and tailmint are stdlib-only; ts4nsnet depends only on the Tailscale SDK.

## Quick start

### Prerequisites

- [butane](https://coreos.github.io/butane/getting-started/) (`brew install butane`)
- Go 1.22+ (the build tool is a Go program)
- A git repo for your container definitions (see [Adding a container](#adding-a-container))
- An SSH deploy key with read access to that repo
- For Tailscale networking: a [Tailscale OAuth client](https://tailscale.com/kb/1215/oauth-clients) with the `tag:tailpod` tag

### Setup

1. **Create `site.env`** from the template:

   ```bash
   cp site.env.example site.env
   ```

   Fill in the required values and any optional values for the overlays you're using:

   | Variable | Required | Purpose |
   |----------|----------|---------|
   | `SSH_PUBKEY` | Always | SSH public key for the `core` user |
   | `QUADSYNC_GIT_URL` | Always | SSH URL of your container definitions repo |
   | `QUADSYNC_GIT_BRANCH` | Always | Branch to track (e.g. `main`) |
   | `TS_API_CLIENT_ID` | With `tailscale.bu` | Tailscale OAuth client ID |
   | `TS_API_CLIENT_SECRET` | With `tailscale.bu` | Tailscale OAuth client secret |
   | `TAILNET_DOMAIN` | With `tailscale.bu` | Your tailnet domain (e.g. `example.ts.net`) |
   | `STORAGE_SMB_*` | With `server.bu` | SMB credentials for persistent storage |

2. **Place your deploy key:**

   ```bash
   cp /path/to/your/key deploy_key
   ```

3. **Enable optional overlays** (if needed):

   ```bash
   # Tailscale networking — included in repo, just add vars to site.env
   # tailscale.bu is committed and used automatically

   # Persistent SMB storage — copy and customize
   cp server.bu.example server.bu
   ```

4. **Build the Ignition manifest:**

   ```bash
   ./build.sh
   ```

5. **Boot a Fedora CoreOS instance** with `tailpod.ign` as the Ignition config.

The VM will download binaries and start the sync timer. Containers from your git repo will be deployed within two minutes. If `tailscale.bu` is present, containers in the `tailscale/` directory join your tailnet automatically. If `server.bu` is present, SMB storage is mounted and per-container directories are created.

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
| `*.container` (root) | None (or `_base.container` if `server.bu` is present) |
| `tailscale/*.container` | `tailscale.container` (+ `_base.container` if `server.bu`) |
| `foo/*.container` | `foo.container` (+ `_base.container` if `server.bu`) |

**Merge rules:**
- `Key=Value` — sets a default (the spec takes precedence if it defines the same key)
- `+Key=Value` — prepends before the spec's values (for multi-value keys like `ExecStartPre`)

### Built-in transforms

**`tailscale.container`** (from `tailscale.bu`) — Adds Tailscale networking. Sets up `ts4nsnet` as the network command, configures Tailscale DNS (`100.100.100.100` + your tailnet domain as search suffix), and prepends `ExecStartPre` steps that mint a fresh ephemeral auth key via `tailmint`.

**`_base.container`** (from `server.bu`) — Applied to all containers. Mounts a per-container named volume at `/data` and runs `storage-init` to create the container's directory on the SMB share.

### Companion templates

Transforms can include companion files. The `_base` transform (from `server.bu`) ships with:

- **`_base-data.volume`** — A named Podman volume for each container, mounted at `/data`.
- **`_base-litestream.container`** — An optional Litestream sidecar that replicates `/data/db.sqlite` to the SMB share. Deployed automatically if the container definitions repo includes a matching file.

## Persistent storage (server.bu)

When `server.bu` is present, tailpod mounts an SMB share at `/var/mnt/storage`. Each container gets:

- A **named Podman volume** (`<name>-data`) mounted at `/data` inside the container
- A **directory on the SMB share** (`/var/mnt/storage/<name>/db_backup`) for off-host backups

The Litestream sidecar, if enabled, continuously replicates SQLite databases from the volume to the SMB backup directory.

## Build pipeline

```
site.env ─→ cmd/build (Go) ─→ substitute ${VAR} ─→ butane --strict ─→ tailpod.ign
tailpod.bu ───┘                                                          ↑
tailscale.bu ─┘ (if present) ─→ same pipeline ─→ JSON merge ─────────────┤
server.bu ────┘ (if present) ─→ same pipeline ─→ JSON merge ─────────────┘
```

The build tool (`cmd/build/`) parses `site.env` as plain `KEY=VALUE`, substitutes only allowlisted variables into `.bu` files using strict `${VAR}` matching, and pipes the result through `butane --strict`. Unknown `${...}` patterns are left intact.

Optional overlays (`tailscale.bu`, `server.bu`) are each processed the same way and merged into the base Ignition at the JSON level. `tailscale.bu` is committed in the repo (Tailscale networking is core to tailpod). `server.bu` is gitignored — copy `server.bu.example` for per-server customization like SMB storage.

## What gets provisioned

### Binaries

| Binary | Version | Source | Purpose |
|--------|---------|--------|---------|
| [quadsync](https://github.com/engie/quadsync) | v0.4 | `tailpod.bu` | Git-sync deployer with INI transforms |
| [ts4nsnet](https://github.com/engie/ts4nsnet) | v0.2 | `tailscale.bu` | Userspace Tailscale networking for containers |
| [tailmint](https://github.com/engie/tailmint) | v0.3 | `tailscale.bu` | Ephemeral auth key minting via OAuth |

### Config files

| Path | Source | Purpose |
|------|--------|---------|
| `/etc/quadsync/config.env` | `tailpod.bu` | Git URL, branch, transform dir, user group |
| `/etc/quadsync/deploy-key` | `tailpod.bu` | SSH key for the container definitions repo |
| `/etc/quadsync/transforms/tailscale.container` | `tailscale.bu` | Tailscale networking transform |
| `/etc/tailscale/oauth.env` | `tailscale.bu` | OAuth credentials for auth key minting |
| `/etc/sudoers.d/tailmint` | `tailscale.bu` | Constrained sudo for container users to mint keys |
| `/etc/quadsync/transforms/_base.container` | `server.bu` | Storage + data volume transform |
| `/etc/samba/storage-credentials` | `server.bu` | SMB credentials for the storage mount |
| `/etc/sudoers.d/storage-init` | `server.bu` | Constrained sudo for container users to init storage |

### systemd units

| Unit | Source | Purpose |
|------|--------|---------|
| `quadsync-sync.timer` | `tailpod.bu` | Polls for container changes every 2 minutes |
| `quadsync-sync.service` | `tailpod.bu` | Runs `quadsync sync` (oneshot) |
| `var-mnt-storage.mount` | `server.bu` | Mounts SMB share at `/var/mnt/storage` |

## Security model

- **Ephemeral auth keys** — OAuth credentials stay on the host. Each container startup mints a fresh, single-use, short-lived Tailscale auth key. No key reuse.
- **Per-container isolation** — Each container runs as its own non-root Linux user with rootless Podman. Users are auto-created with dedicated subuid/subgid ranges.
- **Constrained sudo** — Container users can only run `tailmint` and `storage-init` with specific argument patterns. The sudoers rules use glob matching to prevent argument injection.
- **Allowlisted substitution** — The build tool only substitutes named, allowlisted variables. Shell evaluation is never used.
- **Credential separation** — `site.env`, `deploy_key`, and `tailpod.ign` are all gitignored. The Ignition manifest is written with mode 0600.

## Pitfalls

- **Ignition is first-boot only.** The VM disk must be fresh — Ignition does not re-run on existing installs.
- **OAuth tag scope** must match the `-tag` passed to tailmint (`tag:tailpod` by default), or the Tailscale API returns HTTP 400.
- **Container users must be non-system** for `useradd` to auto-allocate subuid/subgid ranges. Without these, rootless Podman fails.
- **Directory ownership** under `~<user>/.config/` must belong to that user. Podman refuses to start otherwise. quadsync handles this, but be aware if debugging.
- **`site.env` is required.** The build fails if any of the 3 base variables are missing. Overlay-specific variables are warned about when the overlay file is present.

## License

MIT
