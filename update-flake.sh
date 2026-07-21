#!/bin/sh
# Copyright Tailscale Inc & Contributors
# SPDX-License-Identifier: BSD-3-Clause
#
# Updates SRI hashes for flake.nix.

set -eu

# Update go.toolchain.rev.sri from the pinned toolchain Git revision.
TOOLCHAIN_REV=$(cat go.toolchain.rev)
TOOLCHAIN_URL="https://github.com/tailscale/go/archive/${TOOLCHAIN_REV}.tar.gz"
TOOLCHAIN_HASH=$(nix-prefetch-url --unpack "$TOOLCHAIN_URL")
TOOLCHAIN_SRI=$(nix hash convert --hash-algo sha256 --to sri "$TOOLCHAIN_HASH")
printf '%s' "$TOOLCHAIN_SRI" > go.toolchain.rev.sri

OUT=$(mktemp -d -t nar-hash-XXXXXX)
rm -rf "$OUT"

go mod vendor -o "$OUT"
SHA=$(go run tailscale.com/cmd/nardump --sri "$OUT")
rm -rf "$OUT"

# Write the hash to go.mod.sri, which flake.nix reads via fileContents
printf '%s' "$SHA" > go.mod.sri
