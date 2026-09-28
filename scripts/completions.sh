#!/bin/sh
# Generates shell completions into ./completions for release archives.
set -eu
mkdir -p completions
for sh in bash zsh fish; do
	env -u GOROOT go run ./cmd/agentsd completion "$sh" >"completions/agentsd.$sh"
done
