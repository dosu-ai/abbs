# Releasing ABBS

ABBS releases are automated. Maintainers merge ordinary pull requests, then
merge the generated Release Please pull request when its changelog is ready.
There are no hand-created version tags and published tags are never moved.

## Version policy

Squash-merge pull request titles must use Conventional Commit syntax. Release
Please applies the following SemVer policy:

| Title or footer | Release impact |
|---|---|
| `fix: ...`, `perf: ...` | patch |
| `feat: ...` | minor |
| `type!: ...` or a `BREAKING CHANGE:` footer | major |
| all other allowed types | no release by themselves |

The accepted non-releasing types are `docs`, `refactor`, `test`, `build`, `ci`,
`chore`, and `revert` (a revert is included in notes when a release is already
needed). Commits whose changed files are all below `web/` or `cfworker/` are
excluded from the Go CLI release. The first release is `v0.1.0`; subsequent
versions are derived from commits since the prior Release Please PR.

## Automated flow

1. A push to `main` opens or updates one Release Please PR. That PR updates
   `CHANGELOG.md` and `.release-please-manifest.json`.
2. Merging it creates an immutable `vX.Y.Z` tag and a hidden draft release.
3. GoReleaser reuses that draft and builds six CGO-free archives. It adds
   SHA-256 checksums, one SPDX JSON SBOM per archive, both installers, and a
   keyless Cosign bundle for `checksums.txt`.
4. The exact uploaded bundle is passed to native Linux, Windows, macOS Intel,
   and macOS ARM jobs. Each executes `abbs --version` through the appropriate
   installer, including checksum failure tests.
5. After every native job passes, GitHub attests `checksums.txt`. The workflow
   compares the draft asset list with the tested bundle and publishes it.
6. The `published` event dispatches `abbs-release` to `dosu-ai/homebrew-dosu`.
   The tap automation owns the formula PR, enables auto-merge, and lets its
   macOS Intel/ARM and Linux tests gate the merge.

The release workflow uses GoReleaser `v2.17.1`, Cosign `v3.1.3`, and Syft
`v1.51.0`. GitHub Actions are pinned to full commit SHAs; the readable version
comments are informational. `scripts/check-action-pins.sh` prevents a mutable
action reference from entering a pull request.

## One-time repository setup

Create a narrowly scoped GitHub App named Dosu Release Bot and install it only
on `dosu-ai/abbs` and `dosu-ai/homebrew-dosu`. Grant:

- Contents: read and write.
- Pull requests: read and write.
- Issues: read and write (labels use the issues permission).

Expose its credentials as selected-repository organization secrets named
`ABBS_BOT_CLIENT_ID` and `ABBS_BOT_PRIVATE_KEY`. Builds and
signatures do not use an App key; Cosign uses GitHub's short-lived OIDC token.

Before `v0.1.0`:

- Make `abbs` public.
- Enable immutable releases.
- Protect `main` and require pull requests, the conventional-title check,
  current `ci` jobs, and every `release checks` job.
- Configure squash merging so the pull request title becomes the commit title.
- Protect `homebrew-dosu/main`; require its tap syntax and install/test matrix.
- Configure the tap's `repository_dispatch` receiver for event
  `abbs-release`. The payload contains `tag`, `version`, `release_url`, and
  `checksums_url`. It must update only `Formula/abbs.rb`, open a bot PR, enable
  auto-merge, and rely on the protected tap checks.
- Run `release checks` manually once and confirm all four native runners are
  available to the repository.

`ABBS_DOWNLOAD_BASE` exists only for installer tests and controlled HTTPS
mirrors. Production instructions should use the GitHub release URL.

## Failure recovery

A build or test failure leaves the release as an unpublished draft. Fix the
problem on a pull request and rerun the failed release workflow; GoReleaser
replaces matching draft assets idempotently. If a new workflow run is needed,
dispatch `release` manually with the existing draft tag. The workflow refuses
tags that are not still drafts. Do not publish a partial draft.

Once a release is published it is frozen. Never move or reuse its tag and never
replace its assets. Ship regressions as a new patch release. Native Apple
notarization and Windows Authenticode signing are intentionally deferred; the
checksums, keyless signature, and GitHub provenance cover the current release
trust model.
