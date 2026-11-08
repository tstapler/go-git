# go-git fork for stapler-squad (`ssq/v5`)

This is a public fork of [go-git/go-git](https://github.com/go-git/go-git) used by
[tstapler/stapler-squad](https://github.com/tstapler/stapler-squad).

- **Patches**: `ssq/v5` is upstream `v5.19.3` plus fork-only files (`.github/workflows/ssq-ci.yml`, this
  README) and these Go changes:
  - accept the `worktreeConfig`, `partialClone` and `preciousObjects` repository extensions (`v5.19.3-ssq.2`);
  - git's `RenameLimit` rule in `plumbing/object` rename detection: `too_many_rename_candidates`'s
    product rule instead of `max(added, deleted) > limit`. This changes results only for a non-zero
    `RenameLimit`; `0` still means no limit. `v5.19.3-ssq.4`;
  - the similarity matrix builds each file's similarity index once (bounded cache, 128 MiB, falls
    back to per-pair rebuild) instead of once per source/destination pair; output is identical (differential
    test against the previous implementation). `v5.19.3-ssq.4`;
  - rename pairing by file type, which **does change results for default `RenameLimit` 0 callers**: regular
    and executable files now pair with each other (an executable renamed and edited, or a rename that
    changes the mode, is now detected; upstream v5.19.3 did not), inexact detection needs two
    regular/executable files, and symlinks pair only (exactly) with symlinks. The exact pass is also never
    limited by `RenameLimit`. `v5.19.3-ssq.4`.
- **Oracle-tested against `git diff -M`** (git 2.31 or newer; the tests skip on older gits, whose rename detection differs, such as the 2.11 CI leg) (`plumbing/object/rename_git_parity_test.go`, `rename_cache_test.go`):
  50 and 1100 identical files above the limit; regular/executable/symlink combinations, exact and inexact;
  a duplicate group mixing regular files and symlinks; one 12-file content rename.
- **Git two-phase pairing** (stacked change, `v5.19.3-ssq.5`; tag names are placeholders renamed at tag
  time): detection follows git's `diffcore_rename`:
  1. exact renames: destinations in path order; the unused source with the same basename wins, else the
     first in path order;
  2. `find_basename_matches`: a deleted and an added file whose basename is unique among the remaining
     deleted and among the remaining added files pair when their similarity reaches git's
     `min_basename_score`, the minimum score plus **half the gap to 100%** (`-M50` needs 75%, `-M60` 80%,
     `-M90` 95%), before the rename limit applies;
  3. the content matrix, scored by content only, emulating git directly: per destination in path order a
     4-slot array (`NUM_CANDIDATE_PER_DST`) filled by `record_if_better` over every source (sources that
     cannot match, such as symlinks or files of very different size, count as score 0 and still occupy
     slots), basename equality as the tie-break (`name_score`), then a stable sort in git's `score_compare`
     order. Equal-score ties therefore follow git's slot order.

  **Changed pairings versus upstream v5.19.3 for default `RenameLimit` 0 callers**: (1) among identical
  files the first sorted path is chosen, not the last; (2) a same-basename pair beats a pair chosen by
  directory similarity; (3) the 1% name-similarity blend is gone from content scores, so near-threshold
  pairs can flip; (4) the basename pre-pass runs first and demands a higher score than the matrix.
  Oracle-tested against `git diff -M -l<N>` (git 2.31 or newer): the divergences found in review, 60 small
  randomized fixtures, and 150 broad ones (identical-content groups next to an edited copy, symlink and
  unrelated sources, shared basenames, scores 30/50/60, limits 0 to 1000).
- **Remaining known divergences from git**: content similarity is jgit's hashed-span estimate, not git's
  byte-weighted `estimate_similarity`, so pairs within a few percent of a threshold can differ (the
  fixtures keep clear of those zones); copies (`-C`), break detection (`-B`) and `merge.renameLimit` are not
  modelled. Git 2.11 uses an unstable `QSORT` for the matrix and has no basename pre-pass, so it is not an
  oracle (git 2.31 or newer, with `STABLE_QSORT`, is).
- **Performance note**: the basename pre-pass builds both indexes for each candidate pair and does not use
  the source-index cache, so a failed pre-pass pair costs up to twice the indexing of the cached path.
- **Performance** (stacked change; see `bench/README.md` for the cross-library harness and numbers): rename
  scoring runs on GOMAXPROCS workers over compacted similarity indexes (empty hash slots dropped, so a merge
  walks about a dozen entries instead of 256) with a hashed-size bound that skips pairs which cannot reach the
  minimum score; each blob is read once for hashing and once for diffing (`fileContent` no longer inflates it
  twice); duplicate-content exact-rename groups cost O(1) per destination; and a loose-object cache hit costs a
  `stat` instead of an open and close (the stat keeps shared object caches correct across repositories).
  Results are unchanged (same oracle tests).
- **Pack handle pool** (stacked change): without `KeepDescriptors`, idle packfile handles are kept in a bounded pool
  (`filesystem.Options.MaxIdlePackHandles`, default 32, negative disables) instead of reopening the pack for every
  packed-object read. Handles are borrowed exclusively (no shared seek state between concurrent reads), the least
  recently returned pack is evicted when full, `Close` releases them, and `ObjectStorage.OpenPackHandles` /
  `Options.OnOpenPackHandles` expose the open count. A storage dropped without `Close` releases them when the
  garbage collector finalizes the files. On Windows an idle handle blocks deleting that pack file until `Close`.
  Measured on a 13k-file repository with 45 packs, default open: tree diff 184 to about 15-30 ms, log walk
  172 to about 21-30 ms.
- **gitignore**: `ReadPatterns` no longer descends into a directory ignored by an ancestor's pattern.
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
