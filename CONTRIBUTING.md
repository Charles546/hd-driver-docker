# Contributing

## Release process

> **Release & back-merge.** When the `dev` → `main` promotion PR is merged, HDCI on
> `main` runs semantic-release: it cuts a version, commits the updated
> `CHANGELOG.md` to `main` (message carries `[skip ci]`), and pushes the release
> tag. On the `released` event, HDCI automatically (1) relays the release to
> `honeydipper-registry`, which self-registers the driver manifest, and (2)
> back-merges `main` into `dev` as a new merge commit — release tags stay `main`-only.
> The back-merge is idempotent and needs no manual action. If it ever fails (e.g.
> a CHANGELOG conflict), it is reported in the HDCI UI and via chat notification
> (not as a commit status on the repo); resolve the conflict and sync `dev`
> manually, then the next release proceeds normally.
