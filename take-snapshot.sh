#!/usr/bin/env bash

set -e
set -x

curl -o $(dirname "$0")/link-snapshot.json http://go/_/export
