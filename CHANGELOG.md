# 1.0.0 (2026-11-08)


### Bug Fixes

* address reviewer findings on phase 1 driver ([ce6470a](https://github.com/Charles546/hd-driver-docker/commit/ce6470af066f7f1b36bdf720f409e9c5794653c2))
* configure git auth in release step for semantic-release push ([#7](https://github.com/Charles546/hd-driver-docker/issues/7)) ([6e62e61](https://github.com/Charles546/hd-driver-docker/commit/6e62e61270e0ad3357d82598709b039d39329219))


### Features

* hd-driver-docker phase 1 - run-to-complete job lifecycle ([8545d2f](https://github.com/Charles546/hd-driver-docker/commit/8545d2ffdff4b5c74cbd0bf0ea82ded71b5934d6))
* phase 2 exec, volumes, and get_pod_log_tail streaming RPC ([#2](https://github.com/Charles546/hd-driver-docker/issues/2)) ([9cd47ac](https://github.com/Charles546/hd-driver-docker/commit/9cd47acb9d32643142aa3d7ae9649871182eda51))
* phase 3 config bundle, CI, and release/distribution ([#3](https://github.com/Charles546/hd-driver-docker/issues/3)) ([d6fa9da](https://github.com/Charles546/hd-driver-docker/commit/d6fa9da129bdae21b09de4801b9375cd76613c75)), closes [charles-#pages](https://github.com/charles-/issues/pages)

# CHANGELOG

## [Unreleased]

### CI

- **Automate the `main` → `dev` back-merge after releases.** On the `released`
  event, HDCI back-merges `main` into `dev` as a new merge commit so the
  semantic-release CHANGELOG commit flows back to `dev` automatically, keeping
  future promotion PRs conflict-free.

### Security

- **Upgrade the build to Go 1.27.3** to address the CVE affecting Go 1.26 and
  earlier 1.27 patch releases. The release pipeline builds from `go.mod` (via
  `go-version-file` in `tests.yml` and the release workflow's host build), so
  the shipped release binary is built with Go 1.27.3, closing the CVE in the
  consumable artifact.

All notable changes to this project will be documented in this file. This file
is maintained by semantic-release on each release.
