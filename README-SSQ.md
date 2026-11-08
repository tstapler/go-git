# go-git fork for stapler-squad (`ssq/v5`)

This is a public fork of [go-git/go-git](https://github.com/go-git/go-git) used by
[tstapler/stapler-squad](https://github.com/tstapler/stapler-squad).

- **Patches**: `ssq/v5` is upstream `v5.19.3` plus fork-only files (`.github/workflows/ssq-ci.yml`, this
  README) and these Go changes:
  - accept the `worktreeConfig`, `partialClone` and `preciousObjects` repository extensions (`v5.19.3-ssq.2`);
  - git's `RenameLimit` rule in `plumbing/object` rename detection: `too_many_rename_candidates`'s
    product rule instead of `max(added, deleted) > limit`. This changes results only for a non-zero
    `RenameLimit`; `0` still means no limit. `v5.19.3-ssq.4`;
  - `buildSimilarityMatrix` builds each destination's similarity index once (bounded cache, 128 MiB, falls
    back to per-pair rebuild) instead of once per source/destination pair; output is identical (differential
    test against the previous implementation). `v5.19.3-ssq.4`;
  - rename pairing by file type, which **does change results for default `RenameLimit` 0 callers**: regular
    and executable files now pair with each other (an executable renamed and edited, or a rename that
    changes the mode, is now detected; upstream v5.19.3 did not), inexact detection needs two
    regular/executable files, and symlinks pair only (exactly) with symlinks. The exact pass is also never
    limited by `RenameLimit`. `v5.19.3-ssq.4`.
- **Oracle-tested against `git diff -M`** (`plumbing/object/rename_git_parity_test.go`, `rename_cache_test.go`):
  50 and 1100 identical files above the limit; regular/executable/symlink combinations, exact and inexact;
  a duplicate group mixing regular files and symlinks; one 12-file content rename.
- **Git two-phase pairing** (stacked change, `v5.19.3-ssq.5`): detection follows git's `diffcore_rename`:
  exact renames (destinations in path order; the unused source with the same basename wins, else the first
  in path order), then `find_basename_matches` (files whose basename is unique among remaining deleted and
  among remaining added files pair when similar enough, before the rename limit applies), then the
  similarity matrix scored by content only (basename equality is a tie-break, as git's `name_score`), with
  at most 4 candidate sources per destination (`NUM_CANDIDATE_PER_DST`).
  **Changed pairings versus upstream v5.19.3 for default `RenameLimit` 0 callers**: (1) among identical
  files the first sorted path is chosen, not the last; (2) a same-basename pair beats a pair chosen by
  directory similarity; (3) the 1% name-similarity blend is gone from content scores, so near-threshold
  pairs can flip; (4) an unlimited caller never sees the limit, but the basename pre-pass now runs first.
  Oracle-tested against `git diff -M -l<N>`: the three divergences found in review and 60 randomized
  fixtures (duplicate groups, shared basenames, cross-directory moves, small limits).
- **Remaining known divergences from git**: content similarity is jgit's hashed-span estimate, not git's
  byte-weighted `estimate_similarity`, so pairs within a few percent of the threshold can differ; equal-score
  ties in the inexact matrix beyond the basename tie-break are resolved to the first sorted path (git's
  `qsort` order is unspecified); copies (`-C`), break detection (`-B`) and `merge.renameLimit` are not
  modelled.
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
