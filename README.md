# pbsync

Automatically copies generated proto sources to your repo after you
build them with Bazel.

Currently supports most Go protos, and TypeScript protos (using pbjs).

## Usage

Install it as a [bb](https://buildbuddy.io/cli/) plugin:

```shell
bb install --user bduffany/pbsync
```

## Pre-requisites

- `go` 1.19 or higher
- `make`
