# Tailpod — Unified Uber Review (Claude + Codex + Gemini)

Generated: 2026-02-05  
Repo commit: f1c51318dca2fa12a675acf4d76b0cdb82610e56

This document merges three LLM-produced reviews into a single, consistently formatted report.
It preserves all observations and recommendations, with a unified prioritization at the top.

## Inputs
- `claude_review.md`
- `codex_review.md`
- `gemini_review.md`

## Priority scale
- **P0 — Must fix:** exploitable security issues or high-probability destructive failure modes.
- **P1 — Should fix:** correctness/reliability issues that can cause outages, drift, or hard-to-debug behavior.
- **P2 — Nice to have:** maintainability/operability improvements and sharp edges.
- **P3 — Notes:** confirmations, tradeoffs, and low-risk observations.

## Executive summary
**Architecture (shared understanding):**
- Fedora CoreOS host provisioned via Ignition (`tailpod.ign` from `tailpod.bu` + secrets overlay).
- A systemd timer triggers `quadlet-deploy sync` periodically (GitOps reconciliation).
- Each workload runs as its own dedicated system user (rootless Podman + Quadlet).
- A transform system merges repo container specs with local defaults (notably a Tailscale transform).
- Tailscale auth keys are minted on-demand via `ExecStartPre` using `sudo /usr/local/bin/tailpod-mint-key`.

**Main strengths:**
- Strong isolation boundary from the “one container per user” model.
- Idempotent reconciliation approach (hash-based apply) + periodic reconciliation.
- Transform INI merge approach is flexible and has test coverage (INI parse/merge tests).
- Secret separation is generally good: long-lived secrets are root-only; short-lived auth keys are per-container-user runtime files.
- `ts4nsnet` is fetched with SHA256 verification in Ignition.

**Main risks / themes:**
- Privilege and ownership coupling (`cusers`) creates both security and lifecycle hazards.
- Timing-based boot/user-manager readiness (`OnBootSec=30s`, polling loops) where explicit dependencies would be clearer and more reliable.
- Weak failure signaling (sync can exit 0 even when deployments fail) and no sync locking.
- A transform/merge edge case can drop required `EnvironmentFile` entries (breaks Tailscale auth injection).
- Dangerous “empty desired state” behavior can delete all workloads/users.

## Unified prioritized findings

### P0 — Must fix

#### P0.1 `tailpod-mint-key` can be used as a privileged write primitive (unconstrained `sudo` + `-output`)
**Sources:** `gemini_review.md` §3.1; `claude_review.md` §4.2–4.3  
**Impact:** A compromised container user (member of `%cusers`) can run `tailpod-mint-key` as root and choose arbitrary output paths. This enables destructive overwrites (e.g. `/etc/shadow`) and can create/chown files in sensitive directories.  
**Key details:**
- The output content is constrained to `TS_AUTHKEY=...` (+ optional `TS_HOSTNAME=...`), which reduces direct code-exec risk, but file overwrite + ownership changes are still high impact.
- OAuth tag scope may limit which tags can be minted, but it doesn’t mitigate arbitrary-path writes.
- `tailpod-mint-key` writes as root and then chowns to `SUDO_UID`, which can unexpectedly change ownership of files in privileged locations if `-output` is not constrained.
**Recommendations:**
- In `tailpod-mint-key`, restrict outputs to a safe directory (e.g. `/run/user/$SUDO_UID/ts-authkeys/`) and reject any `-output` outside it (use `filepath.Clean` + strict prefix check).
- Prefer removing user-controlled filenames entirely: accept only an output directory (or no output flag) and have the tool choose a fixed filename.
- Consider dropping privileges to the calling user before creating/writing the output (or open/write as that user), while keeping only the minimal privileged operation (reading `/etc/tailscale/oauth.env`).
- Tighten `sudoers` to constrain allowed arguments and output patterns (or replace with a wrapper script that passes fixed args).

#### P0.2 `cusers` group is overloaded (privilege grant + “managed user” lifecycle), enabling unintended user deletion
**Sources:** `codex_review.md` Finding 1; `claude_review.md` §9.1  
**Impact:** Any account added to `cusers` for mint-key privileges becomes eligible for reconciliation cleanup. If it’s not present in desired specs, reconciliation can stop its processes, disable linger, and `userdel -r` it.  
**Recommendations:**
- Split into two groups (examples): `qdeploy-managed` (reconciler ownership) and `qdeploy-mintkey` (sudo privilege).
- Set `QDEPLOY_USER_GROUP` to the managed-only group.
- Point the sudoers `%...` rule at the privilege-only group.
- Add an explicit denylist/allowlist in cleanup (protect `root`, `core`, and any configured human/admin users).

#### P0.3 Empty desired state can trigger mass deletion of all workloads/users (no safety net)
**Sources:** `claude_review.md` §5.2  
**Impact:** If the container definitions repo is force-pushed to empty (or otherwise results in `desired == {}`), cleanup treats all current users as stale and deletes everything. This is a common GitOps foot-gun and is especially risky with shallow clones (no local history).  
**Recommendations:**
- Add a guard: if `len(desired)==0 && len(current)>0`, refuse cleanup and emit a loud warning unless an explicit override is configured.
- Alternative safeguards: require N consecutive empty syncs, require a sentinel file/flag, or persist “last known good desired set” in state and compare before allowing mass deletion.

### P1 — Should fix

#### P1.1 Sync exits success even on partial/total failures (error masking)
**Sources:** `codex_review.md` Finding 4; `claude_review.md` §6.5  
**Impact:** systemd sees the sync as successful even when deployments didn’t happen. Monitoring based on unit/timer health can’t detect outages.  
**Recommendations:**
- Accumulate per-container errors and return non-zero if any desired apply fails.
- Optionally emit a structured per-container status report under the state dir for diagnostics.

#### P1.2 No synchronization/locking around `sync` (timer/manual overlap races)
**Sources:** `codex_review.md` Finding 5; `claude_review.md` §8.2  
**Impact:** Overlapping runs can race on git state, hash writes, quadlet rewrites, and user create/delete lifecycle.  
**Recommendations:**
- Add a lock at sync start (e.g. lockfile in state dir); fail fast when locked and rely on the next timer tick.
- Ensure the lock applies to both timer-driven and manual invocations.

#### P1.3 Boot sequencing relies on timing (`OnBootSec=30s`) instead of explicit dependencies
**Sources:** `codex_review.md` Finding 2; `gemini_review.md` §2.1; `claude_review.md` §2.3  
**Impact:** Fixed delays are brittle on slow boots and waste time on fast boots. They also hide the real prerequisite graph (logind, time sync, network reachability).  
**Recommendations:**
- Express prerequisites on `quadlet-deploy-sync.service`, e.g. `After/Wants=network-online.target systemd-logind.service time-sync.target`.
- Consider a one-time bootstrap service `WantedBy=multi-user.target` (keep the timer for periodic reconciliation).
- Keep the note that the timer itself needn’t depend on network; the service should.

#### P1.4 User manager readiness is polled (magic timeout, no backoff) instead of explicitly activated/waited
**Sources:** `codex_review.md` Finding 3; `claude_review.md` §2.2; `gemini_review.md` §2.2  
**Impact:** On slow/loaded systems, first deploy of new users can fail/skip and wait for the next tick; concurrent polling can add contention.  
**Recommendations:**
- After enabling linger, start and wait for `user@<uid>.service` (`systemctl start --wait user@<uid>.service` or equivalent).
- If polling remains, add backoff and improve diagnostics on timeout.

#### P1.5 Transform merge edge case: `EnvironmentFile` defaults can be dropped, breaking Tailscale auth injection
**Sources:** `gemini_review.md` §2.4; `claude_review.md` §3.1 and §7.2  
**Impact:** If a spec defines its own `EnvironmentFile=...`, the transform’s `EnvironmentFile=-%t/ts-authkeys/%N.env` may be omitted (treated as a default), so `TS_AUTHKEY` isn’t loaded and networking fails.  
**Recommendations:**
- Change the transform to use `+EnvironmentFile=...` (prepend) so it always applies, or
- Treat `EnvironmentFile` as cumulative in merge logic (like `ExecStartPre`), and/or document the required pattern clearly.
- Keep the `EnvironmentFile` in `[Service]` (not `[Container]`) and document why the ExecStartPre → EnvironmentFile ordering works.

#### P1.6 Transform network dependency is weak for the mint-key prestart path (user service semantics)
**Sources:** `codex_review.md` Finding 6; `gemini_review.md` §2.1; `claude_review.md` §5.1  
**Impact:** The user service may run mint-key before connectivity is actually reliable, leading to noisy restart loops.  
**Recommendations:**
- Pair `After=` with `Wants=` where appropriate (`network-online.target`).
- Consider an explicit connectivity check before minting (bounded timeout) to avoid hammering the API on boot.
- Consider system-level gating/bridging if user-manager `network-online` semantics prove unreliable.

#### P1.7 Hash-only short-circuit can miss drift (service stopped/missing even when spec unchanged)
**Sources:** `codex_review.md` Finding 7; `claude_review.md` §2.4  
**Impact:** A workload can be “stuck down” while reconcile does nothing because the spec hash didn’t change.  
**Recommendations:**
- On unchanged specs, cheaply verify runtime convergence (quadlet file exists, user manager reachable, service active or restart attempted).
- Only skip when both spec hash and runtime state match expectations.

#### P1.8 External commands and HTTP calls have no explicit timeout bounds
**Sources:** `codex_review.md` Finding 8  
**Impact:** `sync` or `ExecStartPre` can hang indefinitely on stalled commands/network.  
**Recommendations:**
- Add context-based timeouts for `git`, `loginctl`, `systemctl`, and `userdel` paths.
- Set `http.Client{Timeout: ...}` in `tailpod-mint-key`.
- Align systemd `TimeoutStartSec` / retry policy with these bounds.

#### P1.9 Validation exists but is not enforced on the reconciliation path
**Sources:** `codex_review.md` Finding 9; `claude_review.md` §6.2  
**Impact:** Invalid specs fail late and repeatedly with less helpful errors.  
**Recommendations:**
- Run `CheckDir` after sync and before apply; fail sync on validation errors with clear diagnostics.

#### P1.10 Apply order is nondeterministic (Go `map` iteration)
**Sources:** `codex_review.md` Finding 10; `claude_review.md` §5.3  
**Impact:** Makes debugging harder; can destabilize implicit cross-workload assumptions.  
**Recommendations:**
- Iterate desired container names in sorted order.
- For real dependencies, require explicit unit-level dependency metadata.

#### P1.11 Desired-state name collisions can silently overwrite specs
**Sources:** `claude_review.md` §3.4  
**Impact:** If both `repo/foo.container` and `repo/tailscale/foo.container` exist, one silently overwrites the other in the desired state map.  
**Recommendations:**
- Detect and fail on duplicate stems across directories (preferably in validation).
- Consider namespacing or explicit directory scoping rules if duplicates are expected.

### P2 — Nice to have

- **Replace vestigial core artifacts:** `enable-linger-core.service` and `/home/core/.config/containers/systemd` appear unused in the “per-container user” model. (`claude_review.md` §2.1)
- **Cleanup robustness:** `deleteUser` may race with Podman teardown; consider waiting for container exit or systemd state before `userdel -r`. (`claude_review.md` §3.3)
- **Minor ownership sharp edge:** `writeQuadlet` writes the unit file as root after a recursive chown; the new file is root-owned until next chown (self-healing, likely harmless). (`claude_review.md` §3.2)
- **Hardcoded home dir:** `userHome()` returns `/home/<username>`; more robust to resolve via NSS (`getent passwd`). (`claude_review.md` §6.4; `codex_review.md` Finding 11)
- **Transform directory strictness:** currently warns and skips unknown workload directories; consider strict mode to avoid silent ignores. (`codex_review.md` Finding 12)
- **`VERIFY.sh` correctness:** `sudo VAR=... cmd` may lose the env due to `env_reset`; use `sudo env ...` or `sudo -E`. (`claude_review.md` §8.1)
- **`VERIFY.sh` side effects:** verification currently runs `quadlet-deploy sync` (mutating); consider separating read-only checks from apply. (`claude_review.md` §8.2)
- **Butane/jq merge scope:** `build.sh` only merges `.storage.files` and `.passwd.users`; document clearly or expand to other sections if secrets overlays may need them. (`claude_review.md` §6.3; `codex_review.md` Stage 0 note)
- **Duplicate env parsing + quoted value limitations:** `parseEnvFile` exists in two places and doesn’t handle quoted values; document or harden if configs might evolve. (`claude_review.md` §6.1)
- **Deployer update mechanism:** `quadlet-deploy` is installed via Ignition; consider how it should be updated/rolled forward. (`gemini_review.md` §4)
- **Git deploy key dependency:** invalid/rotated deploy key yields a retry loop; consider operational guidance or alerting hooks. (`gemini_review.md` §4)
- **DNS readiness edge:** transform sets `--dns=100.100.100.100`; apps that resolve DNS immediately may fail if tailnet isn’t ready yet (depends on `ts4nsnet` behavior). (`gemini_review.md` §2.3)
- **INI cosmetics:** blank line preservation differences can cause diff noise; largely cosmetic. (`claude_review.md` §7.3)

### P3 — Notes / confirmations

- **Credential model (overall):** long-lived secrets root-only; minted auth keys scoped to per-container runtime and short-lived (noted as 1h default). (`claude_review.md` §4.1)
- **Git SSH host key policy:** `StrictHostKeyChecking=accept-new` is standard TOFU for automation; bundling host keys would be stricter but requires rotation handling. (`claude_review.md` §4.4)
- **Ignition download integrity:** fetching `ts4nsnet` with SHA256 verification is the right pattern. (`claude_review.md` §4.5)
- **Design tradeoffs:** per-container user isolation is a strong security boundary but adds operational complexity (linger, user managers, per-user quadlet dirs). (`claude_review.md` §9.1)
- **Polling vs webhooks:** 2-minute git polling is simple but adds deployment latency; webhooks could reduce latency if needed. (`claude_review.md` §9.2)
- **Two-binary separation:** `quadlet-deploy` stays generic; `tailpod-mint-key` isolates Tailscale OAuth concerns; transform file is the connection point. (`claude_review.md` §9.4)

---

## Merged system lifecycle trace (boot → container)

This is a combined/normalized trace, merging the three reviews’ descriptions.

### Build-time composition (`build.sh`)
1. Build Linux/arm64 binaries (`quadlet-deploy`, `tailpod-mint-key`).
2. Render Butane configs (base + secrets overlays).
3. Merge selected sections into `tailpod.ign` (notably users + storage files; merge scope is selective).

### First boot provisioning (Ignition)
- Creates `cusers` group.
- Creates directories such as:
  - `/etc/quadlet-deploy/transforms/`
  - `/etc/tailscale/`
  - `/var/lib/quadlet-deploy/`
  - (also `/home/core/.config/containers/systemd` as currently written)
- Installs files/binaries such as:
  - `/usr/local/bin/ts4nsnet` (downloaded with hash verification)
  - `/usr/local/bin/quadlet-deploy`
  - `/usr/local/bin/tailpod-mint-key`
  - `/etc/quadlet-deploy/config.env`, `/etc/quadlet-deploy/deploy-key`, transform file(s), oauth env, sudoers entry
- Installs/enables systemd units: `quadlet-deploy-sync.timer`, `quadlet-deploy-sync.service`, `enable-linger-core.service`.

### Host orchestration (systemd)
- `enable-linger-core.service` runs once (currently appears vestigial).
- `quadlet-deploy-sync.timer` triggers `quadlet-deploy-sync.service` (first run after `OnBootSec=30s`, then every ~2 minutes).

### Reconcile (`quadlet-deploy sync`)
For each run:
1. Ensure state dir exists.
2. Clone/fetch/reset the desired repo using deploy key (`GIT_SSH_COMMAND=...`).
3. Load transforms.
4. Build desired spec map (container name → merged quadlet content).
5. Discover current managed users from group membership.
6. For each desired container user:
   - Create user (root), add to managed group; enable linger.
   - If content changed: write quadlet under `~/.config/containers/systemd/`, chown, wait for user manager readiness, `daemon-reload`, restart the generated `.service`, then save hash.
7. Cleanup any current managed users not present in desired state (stop service, remove quadlet, disable linger, stop user slice, `userdel -r`).

### Runtime behavior (Tailscale transform)
- `ExecStartPre` mints an ephemeral auth key via `sudo tailpod-mint-key` into a runtime envfile.
- `EnvironmentFile=-%t/ts-authkeys/%N.env` injects `TS_AUTHKEY`/`TS_HOSTNAME` into the container start.
- Container runs rootless via Podman, using `ts4nsnet` to join the tailnet.

---

## Failure modes (from Claude, normalized)

| Failure | Impact | Recovery |
|---|---|---|
| Network down on first sync | sync blocks on `network-online.target` | timer retries |
| GitHub SSH auth fails | git clone/fetch fails | timer retries; manual fix for persistent issues |
| OAuth/token request fails | `tailpod-mint-key` fails; container won’t start | systemd restarts on failure (per transform) |
| OAuth tag mismatch | same as above | same as above |
| User creation fails | container skipped; hash not saved | retry on next sync |
| User manager timeout | deploy skipped; hash not saved | retry on next sync |
| Image pull/start fails | container fails | systemd restart on failure |
| Quadlet write fails | deploy skipped; hash not saved | retry on next sync |
| `daemon-reload` fails | deploy skipped; hash not saved | retry on next sync |
| Container starts then crashes | hash already saved; treated as deployed | systemd handles restarts; redeploy only on spec change |
| Tailscale API outage | mint key fails; container won’t start | systemd retry loop |
| Desired repo empty | cleanup deletes everything (today) | add safety net (P0.3) |

---

## Source reviews (for provenance)

These are included unmodified for reference.

<details>
<summary><code>claude_review.md</code></summary>

~~~~markdown
# Tailpod Architecture Review

A deep review of the container host management system: its dependency chains, race conditions, security posture, and failure modes.

---

## 1. The Full Dependency Chain: Boot to Running Container

Understanding the system requires tracing the entire lifecycle from VM power-on to a container serving traffic on the tailnet.

### Phase 1: Ignition (first boot only)

Ignition is the atomic genesis event. Everything provisioned here is guaranteed to exist before systemd starts processing units. The ordering within Ignition is fixed: users/groups, then filesystem, then systemd units.

```
vfkit + tailpod.ign
  └─ Ignition runs (before systemd target processing)
       ├─ passwd.groups: creates "cusers"
       ├─ storage.directories:
       │    ├─ /home/core/.config/containers/systemd (core:core)
       │    ├─ /etc/quadlet-deploy/transforms/
       │    ├─ /etc/tailscale/
       │    └─ /var/lib/quadlet-deploy/
       ├─ storage.files:
       │    ├─ /usr/local/bin/ts4nsnet          (0755, fetched from GitHub)
       │    ├─ /usr/local/bin/quadlet-deploy     (0755, local binary)
       │    ├─ /usr/local/bin/tailpod-mint-key   (0755, local binary)
       │    ├─ /etc/quadlet-deploy/config.env    (0644)
       │    ├─ /etc/quadlet-deploy/deploy-key    (0600)
       │    ├─ /etc/quadlet-deploy/transforms/tailscale.container (0644)
       │    ├─ /etc/tailscale/oauth.env          (0600)
       │    └─ /etc/sudoers.d/tailpod-mint-key   (0440)
       └─ systemd.units:
            ├─ enable-linger-core.service (enabled)
            ├─ quadlet-deploy-sync.service (not enabled, triggered by timer)
            └─ quadlet-deploy-sync.timer (enabled)
```

### Phase 2: systemd boot (multi-user.target + timers.target)

```
systemd processes default.target
  ├─ timers.target
  │    └─ quadlet-deploy-sync.timer starts (OnBootSec=30s)
  └─ multi-user.target
       └─ enable-linger-core.service
            After=systemd-logind.service
            ConditionPathExists=!/var/lib/systemd/linger/core
            ExecStart=loginctl enable-linger core
```

### Phase 3: First sync (T+30s)

```
quadlet-deploy-sync.timer fires
  └─ quadlet-deploy-sync.service
       After=network-online.target, Wants=network-online.target
       Environment="GIT_SSH_COMMAND=ssh -i ... -o StrictHostKeyChecking=accept-new"
       ExecStart=/usr/local/bin/quadlet-deploy sync
         ├─ git clone --depth=1 (using deploy key)
         ├─ load transforms from /etc/quadlet-deploy/transforms/
         ├─ scan repo: tailscale/nginx-demo.container
         ├─ merge spec + tailscale.container transform → full quadlet
         ├─ createUser("nginx-demo", "cusers")
         │    ├─ useradd --create-home -s /sbin/nologin -G cusers nginx-demo
         │    └─ loginctl enable-linger nginx-demo
         ├─ writeQuadlet → /home/nginx-demo/.config/containers/systemd/nginx-demo.container
         │    ├─ os.MkdirAll (as root)
         │    └─ chown -R nginx-demo:nginx-demo ~/.config
         ├─ waitForUserManager("nginx-demo") — polls up to 30s
         ├─ systemctl --user -M nginx-demo@ daemon-reload
         ├─ systemctl --user -M nginx-demo@ restart nginx-demo.service
         │    ├─ ExecStartPre: mkdir -p %t/ts-authkeys
         │    ├─ ExecStartPre: sudo tailpod-mint-key → mints ephemeral key → writes %t/ts-authkeys/%N.env
         │    ├─ EnvironmentFile=-%t/ts-authkeys/%N.env  (loads TS_AUTHKEY, TS_HOSTNAME)
         │    └─ ExecStart: podman run --network-cmd-path=ts4nsnet ... nginx:latest
         │         └─ ts4nsnet joins tailnet using TS_AUTHKEY
         └─ saveHash("nginx-demo", content)
```

---

## 2. Dependency Ordering Issues

### 2.1 Timer has no explicit dependency on enable-linger-core.service

**Severity: Low (currently harmless, architecturally confusing)**

`quadlet-deploy-sync.timer` is `WantedBy=timers.target`. `enable-linger-core.service` is `WantedBy=multi-user.target`. There is no ordering relationship between the two. The timer could fire before core's linger is enabled.

This doesn't matter today because the sync service runs as a system service (root), not as a core user service, and container users get their own linger via `createUser()`. But it raises the question: **why does `enable-linger-core.service` exist at all?**

Core's linger and core's `.config/containers/systemd` directory (created by Ignition) are artefacts that suggest an earlier design where core ran containers. In the current architecture, core never runs containers — every container gets its own dedicated user. These are vestigial and create confusion about what depends on what.

**Recommendation:** Remove `enable-linger-core.service` and the `/home/core/.config/containers/systemd` directory from `tailpod.bu` unless core actually needs a user-level systemd instance for something else.

### 2.2 waitForUserManager uses polling instead of explicit dependency

**Severity: Medium (works, but fragile under load)**

After `createUser` calls `loginctl enable-linger`, the new user's systemd instance starts asynchronously. The code polls `systemctl --user -M user@ is-system-running` every second for up to 30 seconds (`system.go:82-90`).

This is a time-based retry where an explicit dependency would be possible. systemd provides `systemctl --user -M user@ --wait is-system-running` but more importantly, the enable-linger operation could be followed by a blocking wait on the `user@<uid>.service` unit:

```
systemctl start --wait user@$(id -u nginx-demo).service
```

The polling approach works but has two weaknesses:
1. The 30-second timeout is a magic number. Under heavy load (many users being created simultaneously), all of them poll concurrently, and the user managers contend for resources.
2. There's no backoff — it's a tight 1-second loop making `systemctl` calls.

If the timeout expires, the deploy is skipped and retried on the next 2-minute sync cycle. This retry is correct, but the delay adds latency to the first deploy of a new container.

### 2.3 No ordering between timer first-fire and network availability

**Severity: None (correctly handled)**

The timer fires at `OnBootSec=30s` with no network dependency. This is correct: the *timer* doesn't need the network — the *service* does. `quadlet-deploy-sync.service` has `After=network-online.target` and `Wants=network-online.target`, so when the timer triggers the service, systemd ensures the network is up before running ExecStart. If the network isn't ready, the service activation blocks (not the timer).

### 2.4 Hash saved only after full deploy succeeds

**Severity: None (correct design)**

The `saveHash()` call in `reconcile.go:158` is placed after `restartService()` succeeds. If any step in the deploy pipeline fails (writeQuadlet, waitForUserManager, daemonReload, restartService), the hash is not saved, and the next sync cycle will retry. This is the documented design decision ("Reconcile on every sync: Don't short-circuit when git hasn't changed — failed deploys need to retry").

However, this creates an asymmetry: once the hash IS saved, a container that starts successfully but then crashes is considered "deployed". It won't be re-deployed unless the spec changes. Ongoing restarts are delegated to systemd's `Restart=on-failure` in the transform. This is a reasonable separation of concerns but should be understood.

---

## 3. Race Conditions and Timing Hazards

### 3.1 EnvironmentFile + ExecStartPre: the auth key delivery pattern

**Severity: Low (works, but subtle and poorly documented)**

The tailscale transform relies on a subtle systemd behavior:

```ini
[Service]
+ExecStartPre=mkdir -p %t/ts-authkeys
+ExecStartPre=sudo /usr/local/bin/tailpod-mint-key ... -output %t/ts-authkeys/%N.env
EnvironmentFile=-%t/ts-authkeys/%N.env
```

The `-` prefix on EnvironmentFile means "don't fail if missing". The critical question is: **when does systemd read EnvironmentFile relative to ExecStartPre?**

systemd assembles the execution environment for each forked process. The EnvironmentFile is re-read each time a new process is spawned. So the sequence is:
1. Fork ExecStartPre[0]: EnvironmentFile doesn't exist yet → skipped (dash prefix) → mkdir runs
2. Fork ExecStartPre[1]: EnvironmentFile still doesn't exist → tailpod-mint-key creates it
3. Fork ExecStart: EnvironmentFile now exists → TS_AUTHKEY loaded → podman gets the key

This works because systemd re-evaluates EnvironmentFile per-exec, not once at service activation. But this is a non-obvious behaviour that could break if systemd ever changes its evaluation strategy, or if a future contributor moves the EnvironmentFile to the [Container] section (where it would become a podman `--env-file` flag evaluated at a different time).

**Recommendation:** Add a comment in `secrets.bu.example` explaining why this ordering works and that EnvironmentFile MUST remain in [Service], not [Container].

### 3.2 writeQuadlet chown race window

**Severity: Very Low**

`writeQuadlet` (`system.go:113-129`) does:
1. `os.MkdirAll(dir, 0755)` — creates dirs as root
2. `chown -R username:username ~/.config` — fixes ownership
3. `os.WriteFile(path, content, 0644)` — writes file as root (root-owned)

Step 3 runs after step 2, so the newly written file is root-owned until the next `writeQuadlet` call (which would re-chown). But since the file is 0644, the user can read it. And `daemon-reload` triggers the quadlet generator which reads the file as root anyway. The user's systemd can also read 0644 files regardless of ownership.

However, there's a more subtle issue: if `writeQuadlet` is called and then the process crashes before `daemon-reload`, the `.config` directory tree has correct ownership but the new quadlet file is root-owned. On the next sync, `writeQuadlet` is called again (no hash saved), and `chown -R` in step 2 would fix it. So this is self-healing.

### 3.3 deleteUser cleanup race with container shutdown

**Severity: Low-Medium**

`deleteUser` (`system.go:94-110`) does:
1. `loginctl disable-linger` — marks user for cleanup
2. `systemctl stop user-{uid}.slice` — stops all user processes
3. `userdel -r` — deletes user and home directory

Between step 2 and step 3, Podman might still be cleaning up container resources (unmounting overlayfs, removing cgroups). If `userdel -r` runs before Podman has fully released resources in `/home/user/`, it could fail with "directory busy" or leave orphaned mounts.

The code handles this gracefully — `userdel` errors are reported but don't stop the cleanup of other containers. And `stopService` is called before `deleteUser` in the cleanup loop (`reconcile.go:165-174`), giving the container some shutdown time. But there's no explicit wait between stopping the service and deleting the user.

**Recommendation:** After `stopService`, consider waiting for the container to fully exit (e.g., `podman wait` or polling `systemctl --user -M user@ is-active service`). Alternatively, accept the current behavior since the next sync cycle would retry `userdel` if it failed.

### 3.4 Container name collision in desired state map

**Severity: Low (latent bug)**

`buildDesired` in `reconcile.go:214-267` builds a `map[string]string` keyed by container name (filename stem). Root-level files are processed first (`rootFiles` loop, line 222), then subdirectory files (`entries` loop, line 236). If both `repo/foo.container` and `repo/tailscale/foo.container` exist, the subdirectory entry silently overwrites the root-level one in the map.

This is a data loss bug — the root-level spec would be lost. The `check` subcommand doesn't catch this because it validates files individually, not cross-directory uniqueness.

---

## 4. Security Review

### 4.1 Credential exposure surface

| Secret | Location | Mode | Owner | Access Pattern |
|--------|----------|------|-------|----------------|
| OAuth client ID/secret | /etc/tailscale/oauth.env | 0600 | root | Read by tailpod-mint-key (via sudo) |
| SSH deploy key | /etc/quadlet-deploy/deploy-key | 0600 | root | Read by git via GIT_SSH_COMMAND |
| Minted auth keys | %t/ts-authkeys/%N.env | 0600 | container user | Written by tailpod-mint-key, read by systemd EnvironmentFile |

The credential model is sound:
- Long-lived secrets (OAuth creds, deploy key) are root-only
- Short-lived secrets (auth keys) are scoped to the container user
- Auth keys are ephemeral (1 hour default) and minted fresh on each container start

### 4.2 sudoers rule allows unconstrained arguments

**Severity: Low**

The sudoers rule (`tailpod.bu:61`):
```
%cusers ALL=(root) NOPASSWD: /usr/local/bin/tailpod-mint-key
```

This allows any user in `cusers` to run `tailpod-mint-key` with **any arguments**. A compromised container user could:
- Mint keys with different tags: `sudo tailpod-mint-key -tag tag:admin ...` (would fail if OAuth client isn't authorized for that tag)
- Write output to arbitrary paths: `sudo tailpod-mint-key -output /etc/cron.d/backdoor ...` (but the output is always `TS_AUTHKEY=...` format, not executable)
- Read arbitrary OAuth configs: `sudo tailpod-mint-key -config /etc/shadow ...` (would fail to parse as env file)

The blast radius is limited because:
1. tailpod-mint-key only writes `TS_AUTHKEY=<key>` and optionally `TS_HOSTNAME=<name>` — not arbitrary content
2. The OAuth client's tag scope limits which tags can be used
3. The output file gets chowned to SUDO_UID (the calling user), so writing to root-owned paths would change their ownership (this is actually a mild concern)

**Recommendation:** Lock down the sudoers rule with argument constraints:
```
%cusers ALL=(root) NOPASSWD: /usr/local/bin/tailpod-mint-key -config /etc/tailscale/oauth.env -tag tag\:tailpod -output /run/user/*/ts-authkeys/*.env -hostname *
```

### 4.3 chownIfSudo writes to arbitrary paths as root, then chowns

**Severity: Low**

`tailpod-mint-key` runs as root (via sudo). `writeOutput` creates directories with `os.MkdirAll(dir, 0700)`, writes a temp file, then calls `chownIfSudo` to change ownership to the calling user. If the sudoers rule doesn't constrain `-output`, a malicious user could trick it into creating directories owned by themselves in sensitive locations.

Example: `sudo tailpod-mint-key -output /etc/systemd/system/evil.env ...` would:
1. Create /etc/systemd/system/ (already exists, no-op)
2. Write a temp file (0600, root-owned)
3. chown temp file to calling user
4. Rename to evil.env (calling user now owns a file in /etc/systemd/system/)

The file content is always `TS_AUTHKEY=...`, not valid systemd syntax, so this is unlikely to cause direct harm. But file ownership in system directories is still undesirable.

### 4.4 StrictHostKeyChecking=accept-new (TOFU)

**Severity: Acceptable**

The Git SSH command uses `StrictHostKeyChecking=accept-new`, which accepts GitHub's host key on first connection and verifies it thereafter. This is standard TOFU for automated systems. The alternative (bundling GitHub's host key in the Ignition config) would be more secure but requires updating the key if GitHub rotates it.

### 4.5 ts4nsnet binary fetched over HTTPS with SHA256 verification

**Severity: None (properly handled)**

```yaml
contents:
  source: https://github.com/engie/ts4nsnet/releases/download/v0.1/ts4nsnet-linux-arm64
  verification:
    hash: sha256-b85a461798dd95e3c6befc9444401e1bfec1701c7ad223f8e770405d9652f40e
```

This is the correct Ignition pattern: fetch over HTTPS and verify the hash. If the download is tampered with or corrupted, Ignition will refuse to provision the file and the boot fails (which is the right behavior).

---

## 5. Failure Mode Analysis

### 5.1 What happens when things go wrong

| Failure | Impact | Recovery |
|---------|--------|----------|
| Network down at T+30s | sync service blocks on network-online.target | Timer retries at T+30s+2min |
| GitHub SSH auth fails | git clone/fetch fails, sync errors out | Timer retries; manual investigation needed for persistent failures |
| OAuth token request fails (bad credentials) | tailpod-mint-key fails in ExecStartPre; container won't start | systemd Restart=on-failure retries every 10s; sync hash already saved |
| OAuth tag mismatch | Same as above (HTTP 400 from Tailscale API) | Same as above |
| User creation fails (useradd error) | `continue` skips this container; hash not saved | Retry on next sync cycle |
| User manager timeout (>30s) | Deploy skipped; hash not saved | Retry on next sync — user manager should be up by then |
| Container image pull fails | ExecStart (podman run) fails | systemd Restart=on-failure retries |
| Quadlet file write fails | Deploy skipped; hash not saved | Retry on next sync |
| daemon-reload fails | Deploy skipped; hash not saved | Retry on next sync |
| Container starts then crashes | Hash IS saved; quadlet-deploy considers it deployed | systemd Restart=on-failure handles ongoing restarts; re-deploys only on spec change |
| Tailscale API outage | Auth key minting fails; container won't start | systemd Restart=on-failure retries every 10s |
| Git repo deleted/empty | desired state is empty; ALL containers cleaned up | Working as designed, but potentially destructive — see section 5.2 |

### 5.2 Dangerous failure: git repo becomes empty

If the container definitions repo is force-pushed to an empty state, `buildDesired` returns an empty map. The cleanup loop (`reconcile.go:162-176`) then treats ALL current containers as stale and deletes every user and their containers. There is no confirmation, no minimum-count check, no "don't delete everything" safety net.

This is a valid concern for any gitops system. The shallow clone (`--depth=1`) means there's no local history to fall back on.

**Recommendation:** Add a safety check: if `len(desired) == 0` and `len(current) > 0`, log a warning and refuse to clean up. Or require a minimum ratio of desired-to-current before allowing mass deletion.

### 5.3 Partial deploy state across sync failures

The deploy loop processes containers sequentially from a `map[string]string` iteration (non-deterministic order in Go). If the process crashes mid-loop, some containers will have been deployed (hashes saved) and others won't. This is fine — the next sync picks up where it left off. But the non-deterministic iteration order means the set of "deployed before crash" varies between runs. This doesn't cause correctness issues but makes debugging harder.

---

## 6. Code Quality Observations

### 6.1 Duplicated parseEnvFile

`parseEnvFile` is implemented twice: in `quadlet-deploy/reconcile.go:57-71` and `tailpod-mint-key/main.go:142-154`. They are functionally identical. Neither handles quoted values (e.g., `KEY="value with spaces"` would set the value to `"value with spaces"` including the quotes).

This works today because none of the env files use quoted values. But it's a divergence from how shells and systemd parse env files, and a trap for future configuration changes.

The duplication is a consequence of the "two separate Go modules, stdlib only" design. Extracting a shared library would add a dependency between the modules. This is a reasonable tradeoff for two small binaries, but the behavioral limitation should be documented.

### 6.2 check subcommand not called during sync

`CheckDir` (`check.go`) validates container specs: filename is a valid Linux username, `[Container]` section exists with `Image=`, `ContainerName` matches filename. But `Sync` (`reconcile.go`) never calls it. Invalid specs pass through `buildDesired` and fail at deploy time with less informative errors.

**Recommendation:** Call `CheckDir` on the repo after git sync, before `buildDesired`. Reject invalid specs early with clear error messages.

### 6.3 build.sh jq merge is narrowly scoped

The jq merge in `build.sh:21-27` only merges two specific paths:
```
.storage.files += ($s.storage.files // [])
.passwd.users = (group_by/add merge)
```

It does not merge:
- `storage.directories`
- `storage.links`
- `passwd.groups`
- `systemd.units`

If `secrets.bu` ever needs to define groups, directories, or systemd units, the merge will silently drop them. This is a known limitation but could cause hard-to-diagnose issues in the future.

**Recommendation:** Either expand the merge to handle all Ignition config sections, or add a comment in `build.sh` explicitly listing what IS and ISN'T merged with a note about why.

### 6.4 userHome hardcodes /home/<username>

`userHome` (`system.go:176-179`) returns `/home/<username>` unconditionally:
```go
func userHome(username string) (string, error) {
    return "/home/" + username, nil
}
```

On FCOS, `useradd --create-home` defaults to `/home/<name>`, so this is correct. But it's fragile — if the system's HOME_DIR default changes or if a user is created with a non-standard home, this breaks silently. The function has an `error` return value suggesting it was intended to do a lookup (e.g., parsing `/etc/passwd`), but the implementation just concatenates strings.

### 6.5 Error handling in reconcile continues past failures

The deploy loop in `Sync` (`reconcile.go:127-159`) uses `continue` after each failure. This means a failure in one container doesn't affect others. But it also means there's no aggregate error reporting — `Sync` returns `nil` (success) even if every single container failed to deploy. The caller (the systemd service) sees a successful exit code.

The journald logs will contain the individual errors, but monitoring systems checking the timer's success/failure status would see all-green even when everything is broken.

**Recommendation:** Collect errors and return a combined error if any deploys failed. This would cause the service unit to exit non-zero, making it visible in `systemctl status` and timer failure tracking.

---

## 7. The Transform System: Detailed Analysis

The INI merge system is the architectural heart of the project. It's well-designed and well-tested, but has some edge cases worth documenting.

### 7.1 Merge order guarantees

Given spec sections [A, B, C] and transform sections [B, C, D]:
1. Result sections: [A, B, C, D] — spec order first, then transform-only sections appended
2. Within merged sections: prepend entries (+Key), then spec entries, then transform defaults

This is verified by `TestMergeFullExample` which checks section ordering and entry ordering within sections.

### 7.2 Key deduplication uses first-match

`sectionHasKey` (`merge.go:93-99`) returns true on the first key match. For multi-value keys (like `ExecStartPre`), this means the transform's default value is only suppressed if the spec has at least one entry with that key. If the spec has `ExecStartPre=foo` and the transform has `ExecStartPre=bar` (not prepend), the transform's value is suppressed because the key exists.

But prepend entries (`+ExecStartPre`) bypass this check entirely — they're always added. This is the correct behavior for multi-value systemd keys where you want to inject commands before the spec's commands.

### 7.3 Blank line preservation

The INI parser stores blank lines as entries with empty Key and the original line text in Raw. The `String()` method (`ini.go:71-96`) checks for trailing double-newlines before adding section separators, preventing double blank lines. This is tested by `TestINIRoundTrip`.

However, the merge logic doesn't explicitly preserve blank lines from either source. Blank lines in spec sections are preserved (they're entries), but blank lines between sections depend on the rendering logic. Transform-only sections appended at the end get a single blank line separator but not the original blank lines from the transform file. This is cosmetic but could cause diff noise.

---

## 8. VERIFY.sh Issues

### 8.1 GIT_SSH_COMMAND may not survive sudo

**Severity: Medium (VERIFY.sh is a development tool, not production)**

```bash
sudo GIT_SSH_COMMAND='ssh -i /etc/quadlet-deploy/deploy-key -o StrictHostKeyChecking=accept-new' quadlet-deploy sync
```

The `VAR=value sudo command` syntax sets VAR for the `sudo` process, but sudo's default `env_reset` policy clears the environment before running the target command. `GIT_SSH_COMMAND` is not in the default `env_keep` list.

This should be:
```bash
sudo env "GIT_SSH_COMMAND=ssh -i /etc/quadlet-deploy/deploy-key -o StrictHostKeyChecking=accept-new" quadlet-deploy sync
```

Or simply:
```bash
sudo -E GIT_SSH_COMMAND='...' quadlet-deploy sync
```

This likely works in practice because the repo was already cloned by the timer (which sets GIT_SSH_COMMAND via the systemd Environment= directive), and the deploy key path happens to work for root's SSH. But it's technically broken.

### 8.2 Verification runs quadlet-deploy sync as a side effect

Step 10 of VERIFY.sh runs `quadlet-deploy sync`, which is a **mutating** operation (creates users, deploys containers). A verification script should ideally be read-only. If run multiple times or at unexpected moments, it could interfere with the timer-driven sync.

Since `quadlet-deploy sync` is idempotent (via hash checking), this is mostly harmless. But it conflates "verify the system is provisioned correctly" with "force a sync now".

---

## 9. Architectural Observations

### 9.1 The "every container gets its own user" model

This is a strong security boundary. Each container runs in its own user namespace with dedicated subuid/subgid ranges. A container escape would land in a low-privilege user account with no access to other containers' filesystems. The `cusers` group membership is the only shared identity.

The tradeoff is operational complexity: user creation, linger management, per-user systemd instances, and per-user quadlet directories all add moving parts. The 30-second user manager startup delay on first creation is the most visible consequence.

### 9.2 Git polling vs. webhooks

The 2-minute polling interval is simple and reliable but introduces latency. A deployment takes at minimum 30 seconds (OnBootSec) after boot and up to 2 minutes after a git push. For a small-scale personal tailnet, this is perfectly fine. For production use, a webhook-triggered sync would reduce deployment latency to seconds.

### 9.3 Single-binary, stdlib-only design

Both Go binaries have zero external dependencies. This makes them trivially cross-compilable, eliminates supply chain risk, and means the binaries are fully static. The tradeoff is reimplementing things like env file parsing and INI handling instead of using well-tested libraries. The test coverage adequately mitigates this risk for the current feature set.

### 9.4 Separation of quadlet-deploy and tailpod-mint-key

The two binaries have cleanly separated concerns:
- `quadlet-deploy` knows about git, users, quadlets, and systemd — but nothing about Tailscale
- `tailpod-mint-key` knows about Tailscale OAuth — but nothing about containers or deployment

The connection point is the transform file, which references `tailpod-mint-key` in `ExecStartPre`. This is a good design: `quadlet-deploy` is genuinely generic and could deploy non-Tailscale containers with different transforms.

---

## 10. Summary of Recommendations

### Must fix
1. **Empty repo safety net** (section 5.2): Don't delete all containers when the desired state is empty. Add a guard in the cleanup path.

### Should fix
2. **Return aggregate errors from Sync** (section 6.5): Make deployment failures visible in the service's exit code, not just logs.
3. **Call CheckDir during sync** (section 6.2): Validate specs before deploying them.
4. **Fix VERIFY.sh sudo env passing** (section 8.1): Use `sudo env "VAR=..."` or `sudo -E`.

### Nice to have
5. **Remove vestigial core user artifacts** (section 2.1): `enable-linger-core.service` and `/home/core/.config/containers/systemd` appear unused.
6. **Document the EnvironmentFile timing** (section 3.1): Add a comment explaining why the ExecStartPre-then-EnvironmentFile pattern works.
7. **Constrain sudoers rule** (section 4.2): Lock down allowed arguments to tailpod-mint-key.
8. **Document build.sh merge scope** (section 6.3): Make explicit what the jq merge does and doesn't handle.
9. **Consider explicit dependency for user manager readiness** (section 2.2): Replace polling with `systemctl start --wait user@<uid>.service` or similar.

---

*Review generated 2026-02-05. Covers all files in the repository at commit f1c5131.*
~~~~

</details>

<details>
<summary><code>codex_review.md</code></summary>

~~~~markdown
# Container Host Management Review (Dependency and Race Analysis)

## Scope and method

This review covers the container host management system in this repository, with emphasis on dependency correctness across:

1. user creation and ownership
2. linger and user manager lifecycle
3. timer and service triggering
4. command sequencing and error propagation
5. transform-driven container startup behavior

I reviewed these implementation files directly:

- `tailpod.bu`
- `secrets.bu.example`
- `build.sh`
- `quadlet-deploy/main.go`
- `quadlet-deploy/reconcile.go`
- `quadlet-deploy/system.go`
- `quadlet-deploy/check.go`
- `quadlet-deploy/merge.go`
- `tailpod-mint-key/main.go`

I also ran tests:

- `cd quadlet-deploy && go test ./...` (pass)
- `cd tailpod-mint-key && go test ./...` (pass; required unsandboxed local listener bind)

## Executive summary

The overall architecture is clear and pragmatic: Ignition provisions host state, a systemd timer drives reconciliation, and `quadlet-deploy` maps desired container files to per-user rootless Quadlet deployments. The transform mechanism is clean and extensible.

The main weaknesses are dependency strictness and failure signaling:

1. critical coupling bug: one group (`cusers`) is used both for privilege (`sudo`) and reconciliation ownership, which can delete non-container users
2. startup ordering relies on timing (`OnBootSec=30s`, polling loops) where explicit dependencies/services should be used
3. per-container failures are swallowed (logged + continue), so systemd sees successful sync even during partial outage
4. no locking around `sync`, so timer/manual overlap can race user lifecycle and git state

## End-to-end dependency trace (as implemented)

### Stage 0: build-time composition

1. `build.sh` compiles Linux/arm64 binaries (`build.sh:12`, `build.sh:14`).
2. Butane renders base and secrets configs (`build.sh:17`, `build.sh:18`).
3. jq merges storage files and passwd users into `tailpod.ign` (`build.sh:21` through `build.sh:27`).

Dependency note:

- build output correctness depends on secrets overlay correctness, but merge logic is selective (users + files only).

### Stage 1: first boot host provisioning

Ignition provisions directories/files and unit definitions from `tailpod.bu` (+ secrets overlay):

1. creates deployment/config directories (`tailpod.bu:19` through `tailpod.bu:23`)
2. installs binaries (`tailpod.bu:26`, `tailpod.bu:34`, `tailpod.bu:40`)
3. writes deploy config (`tailpod.bu:46`)
4. writes sudoers policy for `%cusers` (`tailpod.bu:57` through `tailpod.bu:61`)
5. writes timer/service units (`tailpod.bu:66`, `tailpod.bu:82`, `tailpod.bu:95`)

Dependency note:

- file provisioning is explicit and deterministic.

### Stage 2: systemd host-level orchestration

1. `enable-linger-core.service` starts at `multi-user.target` (`tailpod.bu:79`), ordered after logind (`tailpod.bu:71`), and runs once (`tailpod.bu:72`).
2. `quadlet-deploy-sync.timer` is enabled (`tailpod.bu:96`) and fires first after `OnBootSec=30s` (`tailpod.bu:102`), then every 2m (`tailpod.bu:103`).
3. timer triggers `quadlet-deploy-sync.service`, which depends only on `network-online.target` (`tailpod.bu:86`, `tailpod.bu:87`).

Dependency note:

- first-sync timing is delay-based (`OnBootSec=30s`) rather than expressing all required units as dependencies.

### Stage 3: reconcile run (`quadlet-deploy sync`)

For each run (`quadlet-deploy/reconcile.go`):

1. ensure state dir (`quadlet-deploy/reconcile.go:76`)
2. clone/fetch/reset desired repo (`quadlet-deploy/reconcile.go:81` through `quadlet-deploy/reconcile.go:97`)
3. load transforms (`quadlet-deploy/reconcile.go:100`)
4. build desired spec map (`quadlet-deploy/reconcile.go:106`)
5. discover current managed users from group membership (`quadlet-deploy/reconcile.go:112`, `quadlet-deploy/system.go:159`)
6. for each desired container/user (`quadlet-deploy/reconcile.go:127`):
   - create user if missing (`quadlet-deploy/reconcile.go:130`, `quadlet-deploy/system.go:69`)
   - enable linger for user (`quadlet-deploy/system.go:73`)
   - if content changed, write quadlet and `chown -R ~/.config` (`quadlet-deploy/reconcile.go:142`, `quadlet-deploy/system.go:124`)
   - poll up to 30s for user manager readiness (`quadlet-deploy/reconcile.go:146`, `quadlet-deploy/system.go:82`)
   - `daemon-reload` then restart service (`quadlet-deploy/reconcile.go:150`, `quadlet-deploy/reconcile.go:154`)
   - persist hash (`quadlet-deploy/reconcile.go:158`)
7. cleanup users not in desired map (`quadlet-deploy/reconcile.go:161`): stop service, remove quadlet, disable linger, stop user slice, `userdel -r` (`quadlet-deploy/reconcile.go:165` through `quadlet-deploy/reconcile.go:173`, `quadlet-deploy/system.go:94` through `quadlet-deploy/system.go:109`)

Dependency note:

- user manager startup and several external commands are coordinated by retries/polling and best-effort logging, not strict dependency gating.

### Stage 4: generated container runtime behavior (tailscale transform)

Transform defaults add:

1. `[Unit] After=network-online.target` (`secrets.bu.example:36`)
2. rootless network config via `ts4nsnet` (`secrets.bu.example:39`, `secrets.bu.example:40`)
3. `ExecStartPre` mint-key flow (`secrets.bu.example:43`, `secrets.bu.example:44`)
4. envfile injection (`secrets.bu.example:45`)
5. restart policy (`secrets.bu.example:46`, `secrets.bu.example:47`)

Dependency note:

- auth key minting has runtime dependencies on network reachability, sudo policy, oauth config, and Tailscale API latency.

## Dependency edge matrix

| Upstream element | Downstream element | How dependency is expressed today | Strength | Main risk |
|---|---|---|---|---|
| Ignition provisioning | `quadlet-deploy-sync.service` runtime inputs (`/usr/local/bin/quadlet-deploy`, config, key, transform files) | Ignition writes files before normal boot | explicit | low |
| `quadlet-deploy-sync.timer` | first `sync` execution | `OnBootSec=30s` (`tailpod.bu:102`) | delay-based | brittle on slow/variable boot |
| `quadlet-deploy-sync.service` | network availability | `After/Wants=network-online.target` (`tailpod.bu:86`, `tailpod.bu:87`) | explicit but coarse | network-online does not guarantee git/API reachability |
| `quadlet-deploy-sync.service` | logind readiness | no unit dependency | missing | `loginctl` path can race on boot |
| `createUser` | linger enablement | sequential commands (`useradd` then `loginctl enable-linger`) (`quadlet-deploy/system.go:69`, `quadlet-deploy/system.go:73`) | explicit | none if commands succeed |
| linger enablement | user manager active state | polling loop with timeout (`quadlet-deploy/system.go:82` through `quadlet-deploy/system.go:90`) | delay-based | timeout/partial deploy on slow startup |
| quadlet file write | user manager generator reload | `daemon-reload` (`quadlet-deploy/reconcile.go:150`) | explicit | requires manager to be ready first |
| reload | container service start | `systemctl ... restart <name>.service` (`quadlet-deploy/reconcile.go:154`) | explicit | restart failure currently non-fatal for overall sync |
| transformed user service | mint-key command authorization | sudoers group rule (`tailpod.bu:61`) | explicit | group also drives reconciliation ownership |
| transformed user service | network before mint key | `[Unit] After=network-online.target` in transform (`secrets.bu.example:36`) | weak | no `Wants`, and user manager semantics differ |
| `QDEPLOY_USER_GROUP` membership | cleanup candidate set | `managedUsers()` + cleanup loop (`quadlet-deploy/system.go:159`, `quadlet-deploy/reconcile.go:161`) | explicit | can delete non-container users |
| reconcile loop | timer/service health signal | per-user errors are `continue`; function returns nil (`quadlet-deploy/reconcile.go:178`) | weak | silent partial outage |

## Findings (ordered by severity)

### 1. Critical: `cusers` is overloaded for both authorization and reconciliation ownership

Evidence:

- `cusers` group is globally defined as deployment group (`tailpod.bu:6`, `tailpod.bu:54`).
- Same group is granted sudo access to `tailpod-mint-key` (`tailpod.bu:61`).
- Reconciler treats all members of that group as managed container users (`quadlet-deploy/system.go:159` through `quadlet-deploy/system.go:174`).
- Cleanup deletes any such user absent from desired specs (`quadlet-deploy/reconcile.go:161` through `quadlet-deploy/reconcile.go:173`).

Failure mode:

- If an operator adds a human/admin account to `cusers` to grant mint-key rights, next reconciliation can stop its slice, disable linger, and run `userdel -r`.

Why this is a dependency flaw:

- one mutable group membership controls two unrelated dependency domains: privilege policy and ownership lifecycle.

Recommendation:

1. split into two groups, for example `qdeploy-managed` and `qdeploy-mintkey`.
2. set `QDEPLOY_USER_GROUP` to managed-only group.
3. point sudoers `%...` rule at privilege-only group.
4. add a hard denylist in cleanup (`root`, `core`, explicit configured protected users).

### 2. High: first-sync startup is delay-based (`OnBootSec=30s`) instead of fully dependency-driven

Evidence:

- timer waits fixed 30s after boot (`tailpod.bu:102`).
- sync service only models network readiness (`tailpod.bu:86`, `tailpod.bu:87`).
- sync path uses `loginctl enable-linger` (`quadlet-deploy/system.go:73`) and user manager operations soon after.

Failure mode:

- on slow/loaded boots, logind or user-manager prerequisites may still be unavailable when the 30s timer elapses; reconcile can partially fail and silently defer to next cycle.

Why this is a dependency flaw:

- fixed delay encodes a guess, not the actual prerequisite graph.

Recommendation:

1. add `After=systemd-logind.service` and `Wants=systemd-logind.service` to `quadlet-deploy-sync.service`.
2. consider separate `quadlet-deploy-bootstrap.service` WantedBy `multi-user.target`, with explicit `After=Wants=network-online.target systemd-logind.service`.
3. keep timer for periodic drift reconciliation only.

### 3. High: user-manager readiness is polled with sleep loops instead of explicit activation dependency

Evidence:

- fixed 30 attempts with `time.Sleep(1s)` (`quadlet-deploy/system.go:82` through `quadlet-deploy/system.go:90`).
- called after writing unit (`quadlet-deploy/reconcile.go:146`).

Failure mode:

- if user manager initialization exceeds 30s, deploy is skipped for that user; retries rely on next timer tick.

Why this is a dependency flaw:

- this is explicitly delay-based race handling where an explicit dependency edge can be established.

Recommendation:

1. after user creation, resolve UID and run `systemctl start user@<uid>.service`.
2. wait on `systemctl is-active user@<uid>.service` with bounded timeout.
3. only then run `systemctl --user -M <user>@ daemon-reload/restart`.

### 4. High: reconcile returns success on partial failures (error masking)

Evidence:

- per-user failures log and continue (`quadlet-deploy/reconcile.go:131`, `quadlet-deploy/reconcile.go:143`, `quadlet-deploy/reconcile.go:147`, `quadlet-deploy/reconcile.go:151`, `quadlet-deploy/reconcile.go:155`).
- function still returns nil at end (`quadlet-deploy/reconcile.go:178`).

Failure mode:

- `quadlet-deploy-sync.service` can appear successful while one or more containers were never deployed/restarted.

Why this is a dependency flaw:

- downstream dependency observers (timer health, monitoring, operator workflows) receive false success signals and cannot react.

Recommendation:

1. accumulate per-user errors.
2. return non-zero if any desired user failed apply.
3. optionally expose per-user status file under state dir for diagnostics.

### 5. High: no synchronization lock around `sync` allows concurrent mutation races

Evidence:

- `Sync` has no lock or single-flight guard (`quadlet-deploy/reconcile.go:74` onward).
- command is callable manually and by timer.

Failure mode:

- overlapping runs can race on:
  - git repo operations (`fetch/reset`)
  - hash file writes (`quadlet-deploy/reconcile.go:279` through `quadlet-deploy/reconcile.go:282`)
  - user create/delete lifecycle
  - quadlet file rewrites/reloads

Recommendation:

1. acquire file lock at sync start (`flock` wrapper or in-process lock file).
2. fail fast when lock held, relying on next timer tick.

### 6. Medium: transform network dependency is weak for the mint-key prestart path

Evidence:

- transform adds `After=network-online.target` only (`secrets.bu.example:36`).
- transform does not add `Wants=network-online.target`.
- `ExecStartPre` calls external OAuth/API endpoints (`secrets.bu.example:44`, `tailpod-mint-key/main.go:224`, `tailpod-mint-key/main.go:196`).

Failure mode:

- container service may execute mint-key before reliable connectivity; startup enters failure/retry loop.

Dependency caveat:

- these are user services; `network-online.target` semantics can differ from system manager expectations.

Recommendation:

1. add explicit connectivity check before minting key, or
2. trigger startup from a system-level dependency bridge that guarantees host networking readiness, and
3. if using target dependencies, pair `After=` with `Wants=` where meaningful.

### 7. Medium: hash-only apply logic skips drift remediation when config content is unchanged

Evidence:

- unchanged content short-circuits deploy (`quadlet-deploy/reconcile.go:136` through `quadlet-deploy/reconcile.go:139`).
- no checks for unit file existence/service active state on unchanged path.

Failure mode:

- if runtime drifts (service stopped, missing unit file, failed boot restore) while hash file remains, reconcile does nothing.

Recommendation:

1. include a cheap runtime convergence check on unchanged specs:
   - quadlet file exists
   - user manager reachable
   - service active or restart attempted
2. only skip when both spec hash and runtime state are converged.

### 8. Medium: command and HTTP calls have no explicit timeout bounds

Evidence:

- `run()` uses `exec.Command` without context timeout (`quadlet-deploy/system.go:15`).
- `httpClient` is default with no timeout (`tailpod-mint-key/main.go:61`).

Failure mode:

- sync can hang indefinitely on external commands.
- `ExecStartPre` mint-key can hang on API/network stalls.

Recommendation:

1. use context-based command timeouts for git/loginctl/systemctl calls.
2. set `http.Client{Timeout: ...}` and optionally request-scoped contexts.
3. align systemd service `TimeoutStartSec` with these values.

### 9. Medium: validation exists but is not enforced in reconciliation path

Evidence:

- robust checks are implemented in `quadlet-deploy/check.go`.
- `sync` path never invokes `CheckDir` (`quadlet-deploy/main.go:52` vs `quadlet-deploy/reconcile.go`).

Failure mode:

- invalid file stems/container names reach runtime operations and fail late, repeatedly.

Recommendation:

1. run `CheckDir(config.RepoPath)` before applying desired state.
2. fail sync on validation errors with clear diagnostics.

### 10. Medium: random apply order (`map` iteration) can destabilize cross-container startup assumptions

Evidence:

- desired state is a map and iterated directly (`quadlet-deploy/reconcile.go:127`).

Failure mode:

- apply/restart ordering is nondeterministic between runs.
- if workloads have implicit dependencies not declared in unit metadata, behavior flaps.

Recommendation:

1. iterate sorted container names for deterministic reconcile behavior.
2. require explicit unit-level dependencies for true startup ordering guarantees.

### 11. Low: hardcoded home path assumption can break portability

Evidence:

- `userHome()` returns `"/home/" + username` unconditionally (`quadlet-deploy/system.go:176` through `quadlet-deploy/system.go:179`).

Failure mode:

- if useradd defaults/home policy changes, quadlet files go to wrong location.

Recommendation:

- resolve home from NSS (`getent passwd`) instead of hardcoding.

### 12. Low: transform selection can silently skip whole directories

Evidence:

- if no matching transform for subdirectory, deployer warns and skips it (`quadlet-deploy/reconcile.go:243` through `quadlet-deploy/reconcile.go:245`).

Failure mode:

- newly added directory can be ignored in production if transform file missing/renamed.

Recommendation:

- choose strict mode: fail sync on unknown workload directories unless explicitly marked as transform-less.

## Explicit dependency opportunities to replace delay/race workarounds

### Opportunity A: replace boot delay with unit dependencies

Current:

- `quadlet-deploy-sync.timer` starts first run after `OnBootSec=30s` (`tailpod.bu:102`).

Better:

1. model actual prerequisites in `quadlet-deploy-sync.service`:
   - `After=network-online.target systemd-logind.service`
   - `Wants=network-online.target systemd-logind.service`
2. optionally add a one-time bootstrap service tied to `multi-user.target`.
3. keep timer periodic interval independent of boot race assumptions.

### Opportunity B: replace 30-second user-manager poll loop with explicit service activation

Current:

- `waitForUserManager()` retries for up to 30s (`quadlet-deploy/system.go:82` through `quadlet-deploy/system.go:90`).

Better:

1. create user
2. `loginctl enable-linger`
3. `systemctl start user@<uid>.service`
4. wait for `is-active`
5. then `daemon-reload` and `restart`

### Opportunity C: replace eventual retries with hard failure signals

Current:

- per-user failures are logged but overall sync succeeds (`quadlet-deploy/reconcile.go:178`).

Better:

1. if desired state cannot be applied for any target user, return non-zero.
2. allow timer retry, but preserve error signal for observability and alerting.

## Additional test and observability gaps

1. `quadlet-deploy` tests focus on INI parse/merge; no tests for reconcile/user/systemd orchestration paths.
2. no tests for failure scenarios:
   - logind unavailable
   - user manager slow-start >30s
   - concurrent sync invocations
   - stale hash with stopped service
3. no lock contention metric or state report.
4. no structured status output per container/user.

## Prioritized hardening plan

1. split managed-user and sudo-privilege groups (critical safety fix)
2. add sync locking and partial-failure nonzero exit
3. convert boot and user-manager readiness from delay-based to explicit dependencies
4. add timeout bounds for all external command and HTTP operations
5. add reconciliation tests for lifecycle/failure paths
6. add drift remediation checks when hash unchanged

## Net assessment

The foundation is solid and the code is readable, but current behavior still depends on timing luck in several places where explicit dependency modeling is possible. The highest-risk issue is not a race, but ownership coupling (`cusers`) that can delete the wrong accounts. After that is addressed, converting delay-based sequencing to explicit dependencies and adding strong failure signaling will materially improve determinism and operational safety.
~~~~

</details>

<details>
<summary><code>gemini_review.md</code></summary>

~~~~markdown
# System Review: Tailpod Container Host Management

## 1. Architecture Overview

The system implements a GitOps-based container management solution for Fedora CoreOS, utilizing **Quadlet** (Podman systemd generator) and **Tailscale** for networking.

*   **Host Configuration (`tailpod.bu`)**: Sets up the base OS, including the `cusers` group, rootless container storage structure, and systemd units for the deployment agent.
*   **Deployment Agent (`quadlet-deploy`)**: A Go binary that synchronizes container definitions from a Git repository, merges them with local "transform" configurations (e.g., for Tailscale sidecars), and deploys them as rootless Quadlets for specific users.
*   **Identity & Networking**: Each container runs as a dedicated system user. Tailscale authentication keys are minted on-demand using a helper binary (`tailpod-mint-key`) via `ExecStartPre`, leveraging a shared OAuth client secret.
*   **Networking Strategy**: Uses `slirp4netns` with a helper `ts4nsnet` to bridge the rootless container into the Tailnet, configured via the transform.

## 2. Dependency & Race Condition Analysis

The system relies on a mix of explicit systemd dependencies and implicit timing assumptions.

### 2.1. The "Sleep" Dependency (`OnBootSec=30s`)
**Status: Suboptimal / Fragile**

The `quadlet-deploy-sync.timer` includes:
```ini
[Timer]
OnBootSec=30s
```
This 30-second delay is a "magic number" used to avoid race conditions at boot, likely intending to wait for:
1.  Network connectivity.
2.  System time synchronization (critical for Git/TLS).
3.  Ignition provisioning completion (specifically secrets).

**Risk:**
*   **Unnecessary Delay:** On fast boots with fast networks, the system sits idle for 30s.
*   **Unreliability:** On slow networks or complex boots, 30s might not be enough, causing the service to fail (though it will retry after 2 minutes).

**Recommendation:**
Replace `OnBootSec=30s` with explicit dependencies in the `[Unit]` section of `quadlet-deploy-sync.service`:
```ini
[Unit]
After=network-online.target time-sync.target
Wants=network-online.target time-sync.target
```
And use `WantedBy=multi-user.target` or a timer with `OnBootSec=0` / `OnStartupSec=0`.

### 2.2. User Creation vs. Manager Start
**Status: Mitigated (mostly)**

The `quadlet-deploy` binary creates users and enables lingering:
```go
run("loginctl", "enable-linger", name)
```
This is an asynchronous request to `systemd-logind`. The user's systemd manager (`user@<uid>.service`) starts asynchronously.
The code attempts to handle this:
```go
func waitForUserManager(name string) error {
    // Polls 'systemctl --user ... is-system-running' for 30s
}
```
**Analysis:**
This polling mechanism effectively mitigates the race condition where we might attempt to interact with the user's systemd instance before it's ready. The check for `running` or `degraded` is robust enough for this purpose.

### 2.3. Tailscale Auth Key Minting vs. Application Start
**Status: Secure but strictly coupled**

The transform adds:
```ini
[Service]
+ExecStartPre=sudo .../tailpod-mint-key ...
```
This ensures the key is minted *before* the container starts.
*   **Race:** If `tailpod-mint-key` fails (e.g., API down), the service fails to start. This is correct behavior (fail-safe).
*   **DNS:** The transform adds `--dns=100.100.100.100`. If the application tries to resolve DNS immediately upon start, and `ts4nsnet` hasn't fully established the tunnel or MagicDNS isn't responding yet, the app might crash. `ts4nsnet` should ideally block until the network is ready, but typically these shims are just connectors.

### 2.4. Transform "Default" Logic Bug
**Status: Bug**

In `quadlet-deploy/merge.go`, the merge logic for `EnvironmentFile` is flawed.
The transform defines:
```ini
EnvironmentFile=-%t/ts-authkeys/%N.env
```
The merge logic treats this as a "default" — it is only added if the key does not exist in the source spec.
**Consequence:** If a user defines `EnvironmentFile=my.env` in their container repo, the Tailscale auth key file is **ignored/dropped**. The container will launch without `TS_AUTHKEY` and networking will likely fail.
**Fix:** The transform should use `+EnvironmentFile` (prepend) or the merge logic should treat `EnvironmentFile` as a cumulative key (like `ExecStartPre`).

## 3. Critical Security Vulnerability

### 3.1. Arbitrary File Overwrite via `tailpod-mint-key`
**Severity: CRITICAL**

The `tailpod-mint-key` binary accepts an `-output` flag to write the generated environment file.
The `sudoers` configuration allows any user in `%cusers` (i.e., **any compromised container user**) to run this binary as root without a password:
```
%cusers ALL=(root) NOPASSWD: /usr/local/bin/tailpod-mint-key
```

**Exploit:**
A compromised container process (running as `u:nginx-demo`) can execute:
```bash
sudo /usr/local/bin/tailpod-mint-key 
  -config /etc/tailscale/oauth.env 
  -tag tag:tailpod 
  -output /etc/shadow
```
This will overwrite `/etc/shadow` (or any other file on the host) with the generated env file content, effectively destroying the system or causing a Denial of Service.

**Fix:**
Modify `tailpod-mint-key` to:
1.  **Enforce Output Directory:** Only allow writing to files inside a specific safe directory (e.g., `/run/ts-authkeys/`) or strictly validate the path ensures it is within the calling user's runtime directory (`/run/user/$UID/`).
2.  **Drop Privileges:** The binary should open the output file *as the calling user* if possible, or `chown` it immediately (which it does, but the open/write happens as root). Ideally, pass the directory as an argument and let the tool control the filename to prevent traversal.

## 4. Other Observations

*   **`quadlet-deploy` Binary Updates:** The binary is installed via Ignition. There is no mechanism to update the deployer itself without re-provisioning the node or manually replacing the binary.
*   **Git Key Dependency:** The system strictly depends on `/etc/quadlet-deploy/deploy-key`. If this key is rotated or invalid, the system enters a failure loop (2min retry).
*   **Orphaned Resources:** `deleteUser` removes the user and home dir. This is clean. However, if `quadlet-deploy` crashes mid-operation, it might leave half-configured users. The `Sync` loop is idempotent, which helps recovery.

## 5. Summary of Recommendations

1.  **Fix Security Hole:** Immediately patch `tailpod-mint-key` to restrict output paths.
2.  **Fix Merge Logic:** Update `quadlet-deploy/merge.go` or the transform to ensure `EnvironmentFile` is cumulative.
3.  **Optimize Boot:** Remove `OnBootSec=30s` and use `After=network-online.target time-sync.target`.
4.  **Strengthen Dependencies:** Add `Wants=network-online.target` to the sync service (it already has `After`).
~~~~

</details>

