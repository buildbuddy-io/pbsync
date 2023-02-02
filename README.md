# pbsync

pbsync allows your IDE to properly resolve protobuf sources that are
built with Bazel.

Currently supports most Go protos and some TypeScript protos (`.d.ts`
definitions built with protobufjs).

## Usage

Install it as a [bb](https://buildbuddy.io/cli/) plugin:

```shell
bb install --user bduffany/pbsync
```

You can get a nice development workflow by combining this plugin with the
`--watch` flag, which will build and copy protos immediately after you
edit them.

## Pre-requisites

- `go` 1.19 or higher
- `make`

## What it does

When you run `pbsync`:

- It looks for all `.proto` files in your repo, using `git ls-files`
  for speed.

- For each proto, it looks for BUILD rules that depend on the proto.

- For supported language-specific rules, it looks for the file in
  the bazel generated source tree, and copies it to the workspace.

NOTE: `pbsync` does NOT build anything for you (yet). It just
copies protos that are already built.

## Thanks

- Original implementation by Vadim Berezniker in https://github.com/vadimberezniker/sgp
- Adapted from Zoey Greer's implementation in https://github.com/tempoz/sgp
