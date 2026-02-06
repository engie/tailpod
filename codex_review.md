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
