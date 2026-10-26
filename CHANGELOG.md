# CHANGELOG

## [Unreleased]

### Security

- **Upgrade the build to Go 1.27.3** to address the CVE affecting Go 1.26 and
  earlier 1.27 patch releases. The release pipeline builds from `go.mod` (via
  `go-version-file` in `tests.yml` and the release workflow's host build), so
  the shipped release binary is built with Go 1.27.3, closing the CVE in the
  consumable artifact.

All notable changes to this project will be documented in this file. This file
is maintained by semantic-release on each release.
