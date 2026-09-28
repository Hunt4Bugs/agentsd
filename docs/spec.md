# agentsd — v0.1 Specification

Status: Draft (amended during implementation; see [Amendments](#amendments))
License: Apache-2.0
Platforms: macOS and Linux, with identical behavior (see §3)

## Amendments

Changes made while implementing v0.1. Each is also reflected inline below.

1. **Implementation language: Go** (resolves open question 1). §13.5 lists only the Go install path.
2. **`[runtime]` table renamed to `[runtime_options]`.** The original examples defined `runtime = "exec"` and a `[runtime]` table in the same file. That is invalid TOML: a key cannot be both a string and a table.
3. **Registration model.** `apply` snapshots each validated manifest into `managed.toml`. The daemon runs only registered agents: `reload` re-reads `config.toml` and `managed.toml`, not the raw manifest files. `agent list/show` reports each manifest as `registered`, `pending` (never applied), `changed` (differs from the applied snapshot), or `unregistered` (applied, manifest gone). Running an unregistered agent fails with exit 4 and a hint to run `apply`.
4. **Default `workspace.cwd`** is the run's output directory (`~/out/<agent>/<run-id>/`) when unset. `limits.timeout` defaults to `30m` and `limits.max_concurrent` to `1`.
5. **Runtime argv templates.** `[runtimes.<name>] argv = ["{command}", "-p", "{prompt}", "{args...}"]` overrides an adapter's invocation. Placeholders must be whole elements.
6. **Output logs** store one line per output line: `<UTC timestamp> <text>`. The timestamp is fixed-width, so `logs` can interleave stdout and stderr by timestamp. `logs --timestamps` keeps the prefix.
7. **Policy scan scope (§8).** Scanning only the `write` roots and `AGENTSD_OUT` could never find a write *outside* them. The scan instead walks the AHS roots plus the run's cwd. It reports regular files and symlinks, but not directories, whose mtimes change when an allowed child is created. It is bounded to 200k entries or 10s. Each `policy.violation` event carries `best_effort: true` and the IDs of concurrently active runs.
8. **Signing: cosign keyless** through GitHub OIDC. The checksum file is signed with a Sigstore bundle, `checksums.txt.sigstore.json`, which holds both the signature and the certificate. That replaces `checksums.txt.sig`.
9. **Homebrew: a cask, not a formula.** GoReleaser has deprecated generated formulas in favor of `homebrew_casks`, which cover macOS and Linux. The install command is unchanged. The cask clears the quarantine bit after install, because the binaries are not notarized.
10. **Socket fallback directory.** `/tmp/agentsd-$UID/` (used only when the preferred socket path is too long) is a third location, alongside the §3.3 service file, that is written outside XDG dirs and AHS roots. `uninstall --purge-state` removes it only when the current environment uses it.
11. **Rejected runs are recorded.** A run refused by a precondition (cwd policy, runtime unavailable, or concurrency) gets a run directory with status `rejected`, a reason, and the matching exit code.
12. **Lost runs are not killed.** Their pid and pgid stay in `run.json` for inspection.
13. **Event timestamps and sequence numbers.** Event `ts` values use nanosecond precision and strictly increase within a run, and every event and output line carries a `seq` number. Replay order therefore matches emission order.
14. **Resumable follow.** A follower that falls too far behind is cut off rather than stalling the run. The stream then ends with `stream.truncated` (not `stream.end`), and clients reconnect with `?after=<seq>`. A completed stream ends with `stream.end`, which carries the final status. These two control events are sent only to followers and are never persisted.
15. **Prompts starting with `-`** are passed to `claude-code` and `codex` on stdin, never in argv, so they cannot be parsed as flags.
16. **The process group ends with the run.** After the main process exits, any processes still in its group get SIGTERM, then SIGKILL 2s later. `run.exited` records `group_reaped: true` when that happens. A stop or daemon shutdown that arrives while a run is still being prepared prevents its process from starting, and the run is recorded as `stopped`.

## 1. Purpose

`agentsd` is a single binary that runs and supervises AI agents on a machine you own. It has two parts: a long-running daemon, and a CLI that talks to that daemon. Agents are defined in TOML manifests. Each agent runs on an existing runtime such as Claude Code, Codex, or any executable, and gets a declared workspace, limits, and a recorded history of its runs.

`agentsd` is the reference implementation of the Agent Home Specification (AHS) v0.2: an XDG-native home that humans and agents share, with `~/src`, `~/wiki`, and `~/out` as the only visible roots.

### 1.1 The v0.1 proof

v0.1 is done when this sentence is true on a fresh macOS or Linux machine:

> Clone your config, run `agentsd apply`, and your agents are defined, supervised, and runnable with `agentsd run`.

### 1.2 Non-goals for v0.1

The following are out of scope for v0.1: remote hosts and fleets, any cloud component, multi-user or organizational features, a TUI, scheduled or recurring runs, model hosting or inference, hard sandbox enforcement (policy is advisory, see §8), secret storage (see §9), and compatibility with external agent-definition formats (see §14).

## 2. Design principles

1. **One binary, one name.** `agentsd` is both the daemon (`agentsd daemon`) and the client. There is no separate `agentctl`.
2. **XDG-native.** All state lives under XDG base directories. The daemon never writes to `~` directly except the AHS visible roots, and only where an agent's policy allows it.
3. **Declarative where it matters.** Agent definitions are files. `plan` shows what `apply` will change, and `apply` only touches files it manages.
4. **Runtime-neutral, not lowest-common-denominator.** Adapters wrap runtimes. They do not abstract away features. Options specific to a runtime pass through unchanged.
5. **Honest policy.** Advisory mode reports; it does not pretend to prevent anything.
6. **Scriptable.** Every command supports `--json`. Exit codes are stable (§11).
7. **Degrades without the daemon.** Read-only and diagnostic commands work when the daemon is down.

## 3. Filesystem layout

### 3.1 XDG resolution

Resolution works the same way on both platforms. On macOS, `agentsd` does **not** use `~/Library/Application Support`.

| Variable | Default when unset |
|---|---|
| `XDG_CONFIG_HOME` | `~/.config` |
| `XDG_STATE_HOME` | `~/.local/state` |
| `XDG_DATA_HOME` | `~/.local/share` |
| `XDG_CACHE_HOME` | `~/.cache` |
| `XDG_RUNTIME_DIR` | Linux: as set by the system. macOS or unset: `$XDG_STATE_HOME/agentsd/run` (created with mode `0700`) |

Relative values are ignored, as the XDG spec requires. An explicitly set `XDG_RUNTIME_DIR` is honored on both platforms, and `agentsd` uses `$XDG_RUNTIME_DIR/agentsd`.

### 3.2 Paths

```text
$XDG_CONFIG_HOME/agentsd/
  config.toml                 global config (§5)
  agents/<name>.toml          agent manifests (§6)

$XDG_STATE_HOME/agentsd/
  daemon.log                  daemon log (rotated)
  managed.toml                ledger of files owned by `apply`, and registered agents
  runs/<run-id>/
    run.json                  run metadata and final status
    events.jsonl              event stream (§12.2)
    stdout.log                "<timestamp> <line>" per line
    stderr.log

$XDG_DATA_HOME/agentsd/
  adapters/                   reserved for out-of-tree adapters (post-v0.1)

$XDG_CACHE_HOME/agentsd/      disposable; safe to delete

$XDG_RUNTIME_DIR/agentsd/
  agentsd.sock                API socket (mode 0600, dir 0700)
  agentsd.pid

AHS visible roots
  ~/src                       code
  ~/wiki                      knowledge
  ~/out/<agent>/<run-id>/     per-run output directory
```

**Socket path length.** macOS limits Unix socket paths to 104 bytes, including the terminating NUL. If the resolved socket path is longer, `agentsd` falls back to `/tmp/agentsd-$UID/agentsd.sock` (dir `0700`, ownership verified), and `doctor` reports a warning.

### 3.3 Platform exceptions

A service manager requires one file outside XDG on each platform. These are created only by `agentsd service install`:

| Platform | Service file |
|---|---|
| macOS | `~/Library/LaunchAgents/dev.agentsd.daemon.plist` |
| Linux | `$XDG_CONFIG_HOME/systemd/user/agentsd.service` |

## 4. CLI overview

```text
agentsd <command> [subcommand] [flags] [args]

Setup and diagnostics
  init                 create config dirs, config.toml, and an example agent
  doctor               check environment, paths, runtimes, daemon
  validate [FILE...]   validate config and manifests
  version

Declarative
  plan                 show changes `apply` would make
  apply                reconcile manifests to the machine and daemon

Daemon
  daemon               run the daemon in the foreground
  service install|uninstall|status|start|stop|restart
  status               daemon and agent summary
  reload               tell the daemon to re-read config and manifests

Agents
  agent list
  agent show NAME

Runs
  run NAME [PROMPT]    start a run
  runs list            list runs (alias: ls)
  runs show RUN_ID
  runs stop RUN_ID
  logs RUN_ID          print or follow a run's output

Misc
  completion bash|zsh|fish
  uninstall [--purge-state]   remove service; optionally delete state and cache
```

### 4.1 Global flags

| Flag | Meaning |
|---|---|
| `--json` | Machine-readable output on stdout. Human messages go to stderr. |
| `--config PATH` | Override the `config.toml` path. Manifests are read from `agents/` beside it. |
| `--socket PATH` | Override the daemon socket path. |
| `-q, --quiet` | Errors only. |
| `-v, --verbose` | Repeatable (`-vv`). |
| `--no-color` | Disable color. Color is also disabled when `NO_COLOR` is set or stdout is not a TTY. |

Environment overrides: `AGENTSD_CONFIG`, `AGENTSD_SOCKET`, `AGENTSD_LOG_LEVEL`.

### 4.2 Daemon dependency

| Works without daemon | Requires daemon |
|---|---|
| `init`, `doctor`, `validate`, `version`, `plan`, `agent list/show` (reads manifests), `runs list/show`, `logs` (reads state dir), `completion`, `service *` | `run`, `runs stop`, `logs --follow` on an active run, `reload`, `status` (full) |

`apply` works without the daemon. If the daemon is running, `apply` finishes by sending it a `reload`. Without the daemon, `status` prints a summary from disk and exits 3.

## 5. Global config — `config.toml`

```toml
# $XDG_CONFIG_HOME/agentsd/config.toml
version = 1

[daemon]
log_level = "info"            # error | warn | info | debug | trace
max_concurrent_runs = 4       # global cap across all agents

[ahs]
roots = ["~/src", "~/wiki", "~/out"]
create_missing_roots = true   # `apply` creates them if absent

[policy]
enforcement = "advisory"      # v0.1 accepts only "advisory"

[runs]
retain = 500                  # keep the most recent N runs; older are pruned
retain_days = 90              # and/or prune runs older than this

[runtimes.claude-code]
command = "claude"            # executable name or absolute path
# argv = ["{command}", "-p", "{prompt}", "{args...}"]

[runtimes.codex]
command = "codex"
# argv = ["{command}", "exec", "{prompt}", "{args...}"]
```

Unknown keys are a validation **error**, not a warning. Typos in config should fail loudly. A missing `config.toml` means defaults. `version` is required when the file exists.

## 6. Agent manifests

One file per agent: `$XDG_CONFIG_HOME/agentsd/agents/<name>.toml`. The filename stem must match `name`.

```toml
name = "researcher"
description = "Reads repositories and writes findings to ~/wiki/research"
runtime = "claude-code"       # claude-code | codex | exec

[workspace]
cwd = "~/src"                 # default working directory for runs (default: the run's output dir)
read  = ["~/src", "~/wiki"]
write = ["~/wiki/research"]   # ~/out/<agent>/<run-id>/ is always writable

[limits]
timeout = "30m"               # default 30m
max_concurrent = 1            # default 1

[env]
pass = ["ANTHROPIC_API_KEY"]  # allowlist from the daemon's environment
set  = { RESEARCH_MODE = "deep" }

[runtime_options]
args = []                     # passed through verbatim to the runtime
```

`exec` agents also declare the command to run:

```toml
name = "nightly-lint"
runtime = "exec"

[runtime_options]
command = ["/usr/bin/env", "bash", "-c", "make lint > \"$AGENTSD_OUT/lint.txt\""]
```

### 6.1 Validation rules

- `name` matches `^[a-z][a-z0-9-]{0,62}$` and is unique.
- Paths in `workspace` must expand to locations under a configured AHS root or under `$XDG_*` dirs. Symlinks are resolved, so a link cannot escape a root. Anything else is a validation error, so an agent can never declare write access to arbitrary paths. The one exception is an explicit `workspace.allow_outside_roots = true`, and `doctor` always flags agents that use it.
- `workspace.cwd`, when set, must lie within `read ∪ write`.
- `timeout` is a Go-style duration (`90s`, `30m`, `2h`) and cannot exceed `24h`.
- `runtime` must name a built-in adapter (§7). `runtime_options.command` is required for `exec` and invalid for other runtimes.
- `env.set` may not set `AGENTSD_*` variables.
- An agent whose runtime executable cannot be found is **valid** but **unavailable**. `validate` passes, `doctor` warns, and `run` fails with exit code 7.

## 7. Runtime adapters

An adapter turns (manifest, prompt, run context) into a process spec: argv, env, cwd, stdin. In v0.1, adapters are compiled in.

| Adapter | Default invocation | Prompt delivery |
|---|---|---|
| `claude-code` | `claude -p <prompt> [args...]` | argv, or stdin when `-` is given |
| `codex` | `codex exec <prompt> [args...]` | argv, or stdin when `-` is given (`codex exec -`) |
| `exec` | `runtime_options.command` + `runtime_options.args` | stdin; also `AGENTSD_PROMPT` |

The default invocations are starting points, not guarantees. Upstream CLIs change often, so each adapter's argv template can be overridden in `config.toml` under `[runtimes.<name>]`. `doctor` records the detected runtime version so that breakage is diagnosable. `claude-code` and `codex` runs require a prompt. For `exec`, the prompt is optional.

### 7.1 Run context injected into every run

| Variable | Value |
|---|---|
| `AGENTSD_RUN_ID` | ULID of the run |
| `AGENTSD_AGENT` | agent name |
| `AGENTSD_OUT` | `~/out/<agent>/<run-id>/` (created before start) |
| `AGENTSD_PROMPT` | prompt text (`exec` only) |

The environment is otherwise **minimal**: `PATH`, `HOME`, `USER`, `LANG`, `TERM`, the XDG variables, plus `env.pass` and `env.set`. Nothing else is inherited from the daemon.

## 8. Policy (advisory in v0.1)

In advisory mode, `agentsd`:

1. Refuses to start a run whose `cwd` falls outside the agent's `read ∪ write` set (exit 5). This is the only hard check in v0.1.
2. After a run exits, scans the AHS roots and the run's cwd, and records any file created or modified outside `write ∪ AGENTSD_OUT` that it can attribute by mtime to the run window. Each one becomes a `policy.violation` event. This detection is best-effort and is labeled that way in the output.
3. Surfaces violations in `runs show`, `status`, and `doctor`.

Advisory mode **does not prevent** anything. Enforcement (`sandbox-exec` on macOS; Landlock or bubblewrap on Linux) is a post-v0.1 milestone and will use the same manifest fields, so manifests written now stay valid.

## 9. Secrets

v0.1 stores no secrets. Credentials reach agents only through `env.pass`, which draws from the daemon's environment. On macOS, `service install --env-file` wraps the daemon in a shell that sources the file at start, so the plist never contains the values. On Linux, the same flag sets the systemd unit's `EnvironmentFile`. `agentsd` never logs environment values. The `run.json` file records only the *names* of passed variables.

Keychain and 1Password references (`keychain:...`, `op://...`) are reserved syntax for a later version. Using them in v0.1 is a validation error.

## 10. Command reference

### `agentsd init`

Creates the XDG directories, a commented `config.toml`, and `agents/example.toml` (an `exec` agent that writes a file to `$AGENTSD_OUT`). It never overwrites existing files. `--force` overwrites only `example.toml`.

### `agentsd doctor`

Runs checks and prints each one as `ok`, `warn`, or `fail`. Exits 0 when there are no `fail` results, and 5 otherwise.

- XDG variables resolved, directories exist, and permissions are correct (`0700` on state and runtime dirs)
- Socket path length is within limits
- AHS roots exist
- Config and manifests validate
- Each referenced runtime is on `PATH`, with its version (`claude --version`, `codex --version`)
- Manifests match the applied state
- Service file is installed and the daemon is reachable; daemon and CLI versions match (a mismatch is a `fail`)
- Linux: lingering is enabled when a service is installed
- Agents flagged `allow_outside_roots`
- Unresolved policy violations in the last 7 days

### `agentsd validate [FILE...]`

Validates `config.toml` and every manifest, or only the listed files. Errors include file, line, and key. Exit 0 means valid; exit 5 means invalid.

### `agentsd plan`

Computes the diff between the manifests and the machine:

```text
+ create dir   ~/wiki/research                 (researcher: workspace.write)
+ create dir   ~/out/researcher
~ register     agent researcher                (new)
~ register     agent nightly-lint              (changed: limits.timeout 10m → 20m)
- remove dir   ~/wiki/old                      (no longer needed; empty and created by apply)
- unregister   agent old-bot                   (manifest removed)

Plan: 2 to create, 1 to remove, 2 to register, 1 to unregister.
```

`--exit-code` returns 6 when there are changes, which is useful in CI or git hooks. Invalid config or manifests make `plan` and `apply` exit 5 without acting.

### `agentsd apply`

Performs the plan. It prompts for confirmation when stdin is a TTY; `-y/--yes` skips the prompt. Every file or directory it creates is recorded in `managed.toml`. `apply` never deletes anything it did not create, and it never deletes a non-empty directory. When the daemon is running, `apply` ends by triggering `reload`.

### `agentsd daemon`

Runs in the foreground and logs to stderr and `daemon.log`. This is the mode the service manager uses. On SIGTERM, it stops accepting new runs, sends SIGTERM to active runs, waits `--grace 30s`, then sends SIGKILL and marks those runs `stopped`. On startup, any run that was left `running` from a previous daemon process is marked `lost`. SIGHUP triggers a reload.

### `agentsd service install|uninstall|status|start|stop|restart`

Writes, removes, or controls the launchd plist or systemd user unit. `install` also accepts `--env-file PATH` to wire a secrets env file into the service definition. `install` captures `PATH` and any `XDG_*` variables, so the daemon resolves the same directories and runtimes as the CLI. It prefers a package-manager symlink on `PATH` (such as Homebrew's) over the versioned binary path. The service is controlled only when it was loaded from agentsd's own service file.

### `agentsd status`

```text
daemon    running  pid 4121  up 3d4h  v0.1.0
agents    3 defined, 3 available
runs      1 running, 42 total (7d), 2 failed (7d)
policy    advisory, 0 violations (7d)
```

### `agentsd agent list` / `agent show NAME`

`list` shows name, runtime, availability, registration, and last run status. `show` prints the resolved manifest (with paths and defaults expanded) and the agent's last 5 runs.

### `agentsd run NAME [PROMPT]`

Starts a run. The prompt comes from the positional argument. If it is `-`, the prompt is read from stdin. For `exec` agents, the prompt is optional.

| Flag | Meaning |
|---|---|
| `-d, --detach` | Print the run ID and return immediately. |
| `--cwd PATH` | Override `workspace.cwd`. Must satisfy §8 rule 1. |
| `--timeout DUR` | Override `limits.timeout`, but only downward (raising it is exit 2). |
| `--label K=V` | Attach metadata. Repeatable. |

In the default foreground mode, output streams to the terminal. Ctrl-C stops the run (SIGTERM, then SIGKILL after 10s). A second Ctrl-C kills it immediately. The exit code reflects the run's outcome (§11). With `--json`, events stream as NDJSON.

If the agent is already at `max_concurrent`, the run is rejected with exit 8. There is no queue in v0.1.

### `agentsd runs list`

Filters: `--agent NAME`, `--status running|succeeded|failed|stopped|timed_out|lost|rejected`, `--since 24h`, `--limit N` (default 20).

### `agentsd runs show RUN_ID`

Prints metadata, timings, exit status, output directory, the names of passed environment variables, and policy violations. Accepts unique ID prefixes.

### `agentsd runs stop RUN_ID`

Sends SIGTERM, then SIGKILL after `--grace` (default 10s). The run's status becomes `stopped`.

### `agentsd logs RUN_ID`

Prints `stdout.log` and `stderr.log` interleaved by timestamp. Flags: `-f/--follow`, `--stdout`, `--stderr`, `--events` (prints `events.jsonl` instead), `--timestamps`.

## 11. Exit codes

| Code | Meaning |
|---|---|
| 0 | Success. For `run`: the run succeeded. |
| 1 | Unspecified error |
| 2 | Usage error (bad flags or arguments) |
| 3 | Daemon unreachable |
| 4 | Not found (agent, run) |
| 5 | Validation or policy precondition failed |
| 6 | `plan --exit-code`: changes pending |
| 7 | Runtime unavailable |
| 8 | Concurrency limit reached |
| 10 | Run failed (non-zero exit from runtime) |
| 11 | Run stopped |
| 12 | Run timed out |

## 12. Daemon API

HTTP/1.1 with JSON bodies over the Unix socket. The API is versioned under `/v1`. The CLI is its only supported client in v0.1, but the API is documented so scripts can use it.

**Access control.** The socket is mode `0600` inside a `0700` directory, and the daemon verifies that the peer's UID matches its own (`SO_PEERCRED` on Linux, `LOCAL_PEERCRED` on macOS). There is no other authentication.

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/status` | Daemon health and summary |
| POST | `/v1/reload` | Re-read config and the registered agents |
| GET | `/v1/agents` | List agents |
| GET | `/v1/agents/{name}` | Resolved manifest and availability |
| POST | `/v1/runs` | Start a run: `{agent, prompt?, prompt_stdin?, cwd?, timeout?, labels?}` |
| GET | `/v1/runs` | List runs (query params mirror `runs list`) |
| GET | `/v1/runs/{id}` | Run detail |
| POST | `/v1/runs/{id}/stop` | Stop a run: `{grace?}` |
| GET | `/v1/runs/{id}/events?follow=1[&after=SEQ]` | NDJSON event stream. With `follow`, it replays persisted events and output, streams live events, and ends with `stream.end` or `stream.truncated`. `after` skips events up to that `seq`. |

Errors use the shape `{"error": {"code": "not_found", "message": "...", "run_id": "..."}}`, and each error code maps to an exit code in §11: `bad_request` 2, `daemon_unreachable` 3, `not_found` 4, `invalid` and `policy` 5, `runtime_unavailable` 7, `concurrency_limit` 8, `internal` 1.

### 12.1 Run states

```text
pending → running → succeeded | failed | timed_out | stopped
                  ↘ lost        (daemon restarted while running)
pending → rejected              (policy precondition, runtime unavailable, or concurrency)
```

### 12.2 Events (`events.jsonl`)

```json
{"ts":"2026-09-28T17:02:11.412345678Z","run_id":"01J9...","seq":2,"type":"run.started","data":{"pid":5521,"argv0":"claude"}}
```

Event types: `run.created`, `run.started`, `run.output` (line-level, stream-tagged, only when following), `run.exited`, `run.stopped`, `run.timed_out`, `run.lost`, `policy.violation`, `daemon.reload`. `run.exited` is always emitted when a process ends. `run.stopped` or `run.timed_out` follows it when that is the reason. Followers also receive the stream-control events `stream.end` and `stream.truncated`, which are not persisted.

## 13. Installation and distribution

`agentsd` ships as a single static binary with no runtime dependencies. The agent runtimes it wraps, such as Claude Code and Codex, are installed separately by the user. `agentsd` never installs them.

### 13.1 Release artifacts

Every tagged release (`vX.Y.Z`) publishes the following to GitHub Releases on `Hunt4Bugs/agentsd`:

| Target | Artifact |
|---|---|
| macOS arm64 | `agentsd_X.Y.Z_darwin_arm64.tar.gz` |
| macOS amd64 | `agentsd_X.Y.Z_darwin_amd64.tar.gz` |
| Linux amd64 | `agentsd_X.Y.Z_linux_amd64.tar.gz` |
| Linux arm64 | `agentsd_X.Y.Z_linux_arm64.tar.gz` |
| All | `checksums.txt` (SHA-256) and `checksums.txt.sigstore.json` |

Each tarball contains the `agentsd` binary, `LICENSE`, `README.md`, and generated shell completions (`completions/agentsd.{bash,zsh,fish}`).

Linux binaries are statically linked, so one build works across glibc and musl distributions. macOS binaries are built per architecture. They are not notarized in v0.1 (see §13.8).

The checksum file is signed with cosign keyless signing from the release workflow. Verify it with:

```bash
cosign verify-blob \
  --certificate-identity 'https://github.com/Hunt4Bugs/agentsd/.github/workflows/release.yml@refs/tags/vX.Y.Z' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  --bundle checksums.txt.sigstore.json checksums.txt
```

### 13.2 Install script (macOS and Linux) — primary method

```bash
curl -fsSL https://raw.githubusercontent.com/Hunt4Bugs/agentsd/main/install.sh | sh

# pin a version or directory
curl -fsSL https://raw.githubusercontent.com/Hunt4Bugs/agentsd/main/install.sh | sh -s -- --version v0.1.0 --dir /usr/local/bin
```

The script's behavior:

1. Detects OS and architecture, and refuses to run on unsupported targets.
2. Resolves the latest release, or the one given by `AGENTSD_VERSION=vX.Y.Z`.
3. Downloads the tarball and `checksums.txt`, and **verifies the SHA-256 checksum. It aborts on mismatch.** If `cosign` is on `PATH`, it also verifies the signature. If not, it says it skipped that step.
4. Installs to `${AGENTSD_INSTALL_DIR:-$HOME/.local/bin}`, without `sudo`. It never writes to `/usr/local` unless the user sets `AGENTSD_INSTALL_DIR` explicitly.
5. Warns if the install directory is not on `PATH` and prints the line to add.
6. Prints next steps (`agentsd init`, `agentsd doctor`) and exits. It does **not** run `init` or install the service.

The script is POSIX `sh`, needs only `curl` or `wget`, `tar`, and `shasum` or `sha256sum`, and is kept under 200 lines so it can be read before piping. Users who prefer can download and inspect it first:

```bash
curl -fsSLO https://raw.githubusercontent.com/Hunt4Bugs/agentsd/main/install.sh
less install.sh && sh install.sh
```

**Where the script is served from.** The script lives at `install.sh` in the root of the repo and is fetched directly from GitHub. No other domain or hosting is involved. The script always installs a **released** binary with a verified checksum, even though the script itself comes from `main`. Users who want the script pinned to a specific release can fetch it from that release's tag:

```bash
curl -fsSL https://raw.githubusercontent.com/Hunt4Bugs/agentsd/v0.1.0/install.sh | sh
```

Because this URL runs whatever is on `main`, changes to `install.sh` go through pull requests with shellcheck in CI. The same rule applies to any code that ends up in a user's shell.

Additional script guarantees:

- The whole script is wrapped in `main()`, so a download cut off partway through executes nothing.
- The binary is replaced atomically (written alongside, then renamed), so reinstalling over a running daemon is safe.
- It never edits shell rc files.

### 13.3 Homebrew (macOS and Linux)

```bash
brew install hunt4bugs/tap/agentsd
```

This uses a tap repository, `Hunt4Bugs/homebrew-tap`, whose cask is updated automatically on each release. The cask installs the binary and shell completions.

The cask does **not** define a service. The daemon is managed only through `agentsd service install` (§10), so there is one service definition and one plist label (`dev.agentsd.daemon`). Two competing launchd jobs for the same daemon would fight over the socket.

Upgrade with `brew upgrade agentsd`, then run `agentsd service restart`.

### 13.4 Manual install from GitHub Releases

```bash
VERSION=0.1.0
OS=darwin      # or linux
ARCH=arm64     # or amd64
curl -fsSLO "https://github.com/Hunt4Bugs/agentsd/releases/download/v${VERSION}/agentsd_${VERSION}_${OS}_${ARCH}.tar.gz"
curl -fsSLO "https://github.com/Hunt4Bugs/agentsd/releases/download/v${VERSION}/checksums.txt"
shasum -a 256 --ignore-missing -c checksums.txt   # Linux: sha256sum --ignore-missing -c checksums.txt
tar -xzf "agentsd_${VERSION}_${OS}_${ARCH}.tar.gz"
install -m 0755 agentsd ~/.local/bin/agentsd
```

### 13.5 From source

```bash
go install github.com/Hunt4Bugs/agentsd/cmd/agentsd@latest
```

To build from a clone, run `make build`. Source builds are supported but not the recommended path, because they skip checksum verification against a signed release.

### 13.6 Deferred channels (post-v0.1)

These channels are deferred until someone other than the author is asking for them:

- `.deb` and `.rpm` packages attached to releases, which would install a systemd user unit in `/usr/lib/systemd/user/`
- An AUR package
- A Nix flake
- `homebrew-core` submission, which requires the notability thresholds Homebrew enforces

A container image is intentionally **not** planned. `agentsd` supervises processes on the host and manages host service files, and running it inside a container defeats that purpose.

### 13.7 Post-install setup

The same steps apply on every platform:

```bash
agentsd version
agentsd init                 # creates XDG dirs, config.toml, example agent
agentsd doctor               # verify paths, runtimes, socket length
agentsd apply                # create AHS roots and register agents
agentsd service install      # launchd (macOS) or systemd --user (Linux)
agentsd run example
```

On Linux, the daemon keeps running after logout only if lingering is enabled with `loginctl enable-linger $USER`. `doctor` warns when a service is installed but lingering is off. `service install` prints the command but does not run it, because it may require privileges.

Shell completions are installed by Homebrew. Other install methods do not install them automatically. Users generate them with:

```bash
agentsd completion zsh > "${XDG_DATA_HOME:-$HOME/.local/share}/zsh/site-functions/_agentsd"
```

### 13.8 macOS signing and notarization

v0.1 binaries are not code-signed or notarized. Here is how each install method is affected:

| Method | Gatekeeper impact |
|---|---|
| Homebrew | None; the cask clears the quarantine attribute after install |
| Install script (`curl`) | None; `curl` does not set the quarantine attribute |
| Browser download of the tarball | Quarantined; clear with `xattr -d com.apple.quarantine ./agentsd` |

Notarization requires an Apple Developer Program membership and a signing step in CI. It is planned for when browser-downloaded binaries become a real support burden. It is not planned before then.

### 13.9 Upgrades and uninstall

There is no `self-update` command. The installer that put the binary in place also handles upgrades: Homebrew, the install script (re-run it), or manual replacement. After upgrading, run `agentsd service restart`. `doctor` fails when the running daemon's version differs from the CLI's.

Uninstall happens in two steps, and the second is opt-in so that run history and config are never lost by accident:

```bash
agentsd service uninstall          # stop the daemon, remove the plist/unit
agentsd uninstall --purge-state    # optional: delete $XDG_STATE_HOME/agentsd and $XDG_CACHE_HOME/agentsd
# then remove the binary: brew uninstall agentsd, or rm ~/.local/bin/agentsd
```

`agentsd uninstall` never deletes `$XDG_CONFIG_HOME/agentsd` or the AHS roots (`~/src`, `~/wiki`, `~/out`). Config is user-authored, and the roots hold user and agent work. `--purge-state` refuses to run while a daemon is reachable.

### 13.10 Release automation

Releases are cut from a git tag by CI (GitHub Actions). CI does the following: builds all four targets, generates completions, writes checksums, signs them, publishes the release, and updates the Homebrew tap cask. A release is not published unless the §15 acceptance checklist passes on macOS arm64 and Linux amd64 runners.

## 14. Open questions

1. ~~**Implementation language.**~~ Resolved: Go.
2. **External formats.** Whether to import or export any external `.agents`-style definition format, and which one. Deferred until a format has real adoption. The native TOML manifest is the source of truth either way.
3. **Hermes adapter.** Add it as the fourth built-in adapter, or wait for the out-of-tree adapter interface?
4. **Queueing.** v0.1 rejects runs at the concurrency limit. A queue would be simple to add but brings persistence and fairness questions.
5. **Skills and runtime config placement.** AHS wants skills and runtime configs (Claude Code, Codex) to live in XDG locations. Should `apply` manage those symlinks and files, or is that a separate AHS tool?

## 15. v0.1 acceptance checklist

Automated checks live in `test/e2e` (`make e2e`) and `test/install` (`make install-test`). Both run in CI on macOS and Linux.

- [ ] `init` → `apply` → `service install` → `run example` succeeds on macOS and on one Linux distribution, with identical paths under XDG
- [ ] `doctor` detects a missing runtime, a bad socket path length, and a manifest error
- [ ] `plan` is idempotent: after `apply`, `plan` reports no changes
- [ ] Killing the daemon mid-run marks the run `lost` on restart
- [ ] A run that writes outside `write ∪ AGENTSD_OUT` produces a `policy.violation` event
- [ ] Every command supports `--json`; exit codes match §11
- [ ] No file is written outside XDG dirs, AHS roots, and the §3.3 service file
- [ ] `brew install hunt4bugs/tap/agentsd` and `install.sh` both produce a working binary on macOS arm64 and Linux amd64
- [ ] `install.sh` aborts on a checksum mismatch
- [ ] `agentsd uninstall --purge-state` leaves config and AHS roots untouched
