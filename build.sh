#!/bin/bash

set -e
set -x

export PATH=$(readlink -f $(dirname $0)/../../tool):$PATH

go run github.com/tailscale/mkctr \
  --target="flyio" \
  --base="alpine:3.15" \
  --gopaths="tailscale.io/cmd/golink:/tsgo" \
  --tags="latest" \
  --repos="registry.fly.io/tsgo" \
  --push \
  /tsgo -verbose -sqlitedb /root/golinks.db
