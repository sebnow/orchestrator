#!/bin/sh
# Runs one spike scenario with a Nix-provided python3 (the macOS /usr/bin/python3
# shim needs the Xcode command line tools).
set -eu
exec nix shell nixpkgs#python3 -c python3 "$(dirname "$0")/driver.py" "$@"
