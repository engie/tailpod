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
