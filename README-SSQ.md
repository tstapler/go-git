# go-git fork for stapler-squad (`ssq/v5`)

This is a public fork of [go-git/go-git](https://github.com/go-git/go-git) used by
[tstapler/stapler-squad](https://github.com/tstapler/stapler-squad).

- **Patches**: `ssq/v5` is upstream `v5.19.3` plus fork-only files (`.github/workflows/ssq-ci.yml`, this
  README) and these Go changes:
  - accept the `worktreeConfig`, `partialClone` and `preciousObjects` repository extensions (`v5.19.3-ssq.2`);
  - git-exact `RenameLimit` in `plumbing/object` rename detection: git's `too_many_rename_candidates`
    product rule instead of `max(added, deleted) > limit` (`v5.19.3-ssq.4`). Behaviour change only for a
    non-zero `RenameLimit`; `0` still means no limit;
  - `buildSimilarityMatrix` builds each destination's similarity index once (bounded cache, 128 MiB, falls
    back to per-pair rebuild) instead of once per source/destination pair; output is identical (differential
    test against the old implementation). `v5.19.3-ssq.4`.
  - rename-detection parity with git for the exact pass and file types (`v5.19.3-ssq.4`): the exact pass is
    never limited (the limit only gates the inexact matrix); regular and executable files pair with each
    other, inexact detection needs two regular/executable files, and symlinks pair only exactly with
    symlinks (all verified against `git diff -M`, see `rename_git_parity_test.go`).
- **Module line is unchanged** (`module github.com/go-git/go-git/v5`). The app consumes the fork with a
  `replace` directive, so import paths stay `github.com/go-git/go-git/v5`.
- **Tags**: `vX.Y.Z-ssq.N` (never branch pseudo-versions). `v5.19.2-ssq.0` is the unmodified upstream
  `v5.19.2` commit; `v5.19.3-ssq.1` is upstream `v5.19.3` plus the fork-only files.
- **Remotes**: `origin` = this fork, `upstream` = go-git/go-git. Never open PRs against upstream from here.
- **CI**: `ssq-ci.yml` runs `go test ./...` on push to `ssq/v5`. It needs no secrets.

## Rebase runbook

See the stapler-squad plan, `project_plans/go-git-fork-full-git-replacement/` (ADR-001, ADR-002).
In short: `git fetch upstream --tags`, rebase `ssq/v5` onto the new upstream v5 tag, wait for `ssq-ci`,
tag `vX.Y.Z-ssq.N`, then bump the `replace` line in stapler-squad.

## Rollback

In stapler-squad, delete the `replace github.com/go-git/go-git/v5 => ...` line from `go.mod`, run
`go mod tidy`, and rebuild. See `docs/how-to/roll-back-go-git-fork.md` in stapler-squad.
