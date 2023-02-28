#!/bin/sh
# Copyright Tailscale Inc & Contributors
# SPDX-License-Identifier: BSD-3-Clause
#
# Updates SRI hashes for flake.nix.

set -eu

OUT=$(mktemp -d -t nar-hash-XXXXXX)
rm -rf "$OUT"

go mod vendor -o "$OUT"
SHA=$(go run tailscale.com/cmd/nardump --sri "$OUT")
rm -rf "$OUT"

perl -pi -e "s,vendorSha256 = \".*\"; # SHA based on vendoring go.mod,vendorSha256 = \"$SHA\"; # SHA based on vendoring go.mod," flake.nix
