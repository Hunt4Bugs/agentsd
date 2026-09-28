# AGENTS.md — agentsd

Contract for agents working in this repository (`~/src/agentsd`). Scope: this directory tree.

## What this is

`agentsd`, a Go daemon and CLI that supervises AI agent runs. `docs/spec.md` is the source of truth for behavior. Its **Amendments** section records every deviation from the original draft. When behavior changes, update the spec in the same change.

## Layout

- `cmd/agentsd`: entry point only.
- `internal/`: one package per concern (`config`, `manifest`, `adapter`, `runstore`, `supervisor`, `policy`, `daemon`, `client`, `plan`, `ledger`, `service`, `doctor`, `cli`, and others). No package outside `internal/`.
- `test/e2e`: acceptance suite (build tag `e2e`) that drives the real binary against a temporary HOME. `test/install`: `install.sh` tests against a mock release.
- `install.sh` runs in users' shells. Keep it POSIX `sh`, under 200 lines, wrapped in `main()`, and shellcheck-clean.

## Working rules

- Build and test with `make test` and `make e2e`. The Makefile unsets `GOROOT`, because a stale `GOROOT` export on this machine breaks the Homebrew Go toolchain.
- Keep `go.mod` at `go 1.25.0`, and don't pull dependencies that need a newer Go.
- Every command must support `--json` and return the exit codes in spec §11 (`internal/exitcode`).
- Tests must never touch the real `$HOME`, launchd, or systemd. E2E tests use a short temp HOME (so the socket needs no fallback) and never run `service install`.
- Never write outside XDG dirs, AHS roots, and the §3.3 service file. `test/e2e` checks this.
