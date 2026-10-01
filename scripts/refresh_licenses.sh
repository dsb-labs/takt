#!/usr/bin/env bash

set -e

go tool go-licenses save \
  --one_output \
  --force \
  --ignore "github.com/dsb-labs/takt" \
  --ignore "golang.org/x/sys" \
  --save_path licenses \
  .
