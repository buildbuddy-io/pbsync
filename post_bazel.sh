#!/usr/bin/env bash
set -e

# Build the plugin if it's out of date.
make -s

./pbsync "$BUILD_WORKSPACE_DIRECTORY" &
