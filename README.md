# agentsd

Run and supervise AI agents on a machine you own.

`agentsd` is a single binary: a daemon plus the CLI that talks to it. You define agents in TOML manifests. Each agent runs on an existing runtime (Claude Code, Codex, or any executable) and gets a declared workspace, limits, and a recorded history of its runs. All state lives under XDG directories, the same way on macOS and Linux.

> Clone your config, run `agentsd apply`, and your agents are defined, supervised, and runnable with `agentsd run`.

The full design is in [docs/spec.md](docs/spec.md).

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/Hunt4Bugs/agentsd/main/install.sh | sh
# or
brew install hunt4bugs/tap/agentsd
# or, from source
go install github.com/Hunt4Bugs/agentsd/cmd/agentsd@latest
```

The install script verifies the release checksum, and verifies the cosign signature too when `cosign` is installed. It installs to `~/.local/bin` without `sudo`. Releases sign `checksums.txt` with cosign keyless signing:

```bash
cosign verify-blob \
  --certificate-identity 'https://github.com/Hunt4Bugs/agentsd/.github/workflows/release.yml@refs/tags/vX.Y.Z' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  --bundle checksums.txt.sigstore.json checksums.txt
```

## Quick start

```bash
agentsd init                 # config dirs, config.toml, agents/example.toml
agentsd doctor               # paths, runtimes, socket length
agentsd plan                 # what apply would change
agentsd apply                # create ~/src ~/wiki ~/out, register agents
agentsd service install      # launchd (macOS) or systemd --user (Linux)
agentsd run example
```

## An agent

```toml
# ~/.config/agentsd/agents/researcher.toml
name = "researcher"
description = "Reads repositories and writes findings to ~/wiki/research"
runtime = "claude-code"       # claude-code | codex | exec

[workspace]
cwd = "~/src"
read  = ["~/src", "~/wiki"]
write = ["~/wiki/research"]   # ~/out/<agent>/<run-id>/ is always writable

[limits]
timeout = "30m"
max_concurrent = 1

[env]
pass = ["ANTHROPIC_API_KEY"]  # allowlist from the daemon's environment
set  = { RESEARCH_MODE = "deep" }

[runtime_options]
args = []                     # passed through verbatim to the runtime
```

```bash
agentsd apply
agentsd run researcher "Summarize what changed in ~/src/foo this week"
echo "long prompt" | agentsd run researcher -
agentsd run -d researcher "..."       # detach; prints the run ID
agentsd runs list
agentsd logs -f 01J9
```

Every run gets `AGENTSD_RUN_ID`, `AGENTSD_AGENT`, and `AGENTSD_OUT` (its private output directory) in an otherwise minimal environment.

## Policy is advisory

v0.1 does not sandbox anything. It refuses to start a run whose working directory is outside the agent's declared workspace. After each run, it scans for files written outside `workspace.write` and `AGENTSD_OUT`, and records them as `policy.violation` events, which appear in `runs show`, `status`, and `doctor`. The scan is best-effort and is labeled that way. Enforcement will use the same manifest fields in a later version.

## Secrets

`agentsd` stores no secrets. Credentials reach agents only through `env.pass`, which reads from the daemon's environment. Use `agentsd service install --env-file ~/.config/agentsd/secrets.env` to load an env file into the service. On macOS, the file is sourced at start rather than copied into the plist.

## Scripting

Every command supports `--json`, and exit codes are stable:

| Code | Meaning |
|---|---|
| 0 | success |
| 2 | usage error |
| 3 | daemon unreachable |
| 4 | not found |
| 5 | validation or policy precondition failed |
| 6 | `plan --exit-code`: changes pending |
| 7 | runtime unavailable |
| 8 | concurrency limit reached |
| 10 / 11 / 12 | run failed / stopped / timed out |

## Where things live

```text
~/.config/agentsd/config.toml, agents/*.toml     you edit these
~/.local/state/agentsd/                          runs, managed.toml, daemon.log
~/.local/state/agentsd/run/agentsd.sock          API socket (macOS; $XDG_RUNTIME_DIR/agentsd on Linux)
~/src  ~/wiki  ~/out/<agent>/<run-id>/           AHS visible roots
```

## Uninstall

```bash
agentsd service uninstall
agentsd uninstall --purge-state   # optional: deletes state and cache, never config or ~/src ~/wiki ~/out
brew uninstall agentsd            # or rm ~/.local/bin/agentsd
```

## Development

```bash
make build          # bin/agentsd
make test           # vet + unit tests (-race)
make e2e            # acceptance suite against an isolated HOME
make install-test   # install.sh against a mock release
make lint           # golangci-lint + shellcheck
make snapshot       # goreleaser dry run
```

## License

Apache-2.0
