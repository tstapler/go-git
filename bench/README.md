# Cross-library benchmarks

Compares this fork (go-git), **libgit2** (through `pygit2`) and the **git CLI** on identical repositories.
Not part of the normal build or CI: `bench/libgit2/` is a standalone script, and `bench/ggbench` is plain Go
(no cgo) that only prints timings.

```sh
go build -o /tmp/ggbench ./bench/ggbench
git clone --no-hardlinks <some large repo> /tmp/big-clone          # optional, for the real-repo row
uv run bench/libgit2/bench.py --ggbench /tmp/ggbench --squad-clone /tmp/big-clone [--only "n=1000"]
```

`uv` resolves `pygit2` (which bundles libgit2) from the script header; no system libgit2 is needed. A Go
binding (git2go) was not used because its releases lag behind current libgit2 and need a matching system build.

Operations, each best of 3: tree diff, rename detection (`-M50% -l1000` / `find_similar` threshold 50 and
limit 1000), rename detection plus patch statistics, index read, log walk, reading every blob of a tree, and
status. The git CLI column is wall time including process spawn; the last column subtracts the measured cost
of `git --version`.

Caveats when reading the results:

- libgit2 is a **performance comparator only**. Its similarity algorithm, default thresholds and limits differ
  from git's and it has no basename pre-pass, so rename counts differ (the count is printed next to each time).
  Parity with git is checked by the oracle tests in `plumbing/object`, never by this script.
- Compare equal work only: same thresholds, same repositories. The byte counts of `blob-read-all` differ
  between tools (tar framing) and are informational.
- libgit2 and git keep an object cache; go-git does too (`cache.Object`), but repositories of many
  identical blobs reward whichever tool caches best.
- Numbers are from one machine and one run; use them for ratios, not absolutes.

`ggbench` also prints `cpu=<ms>` (user plus system CPU of the best run). Use it, not wall time, when the machine is
loaded: wall times of the same binary varied 2x to 5x during the measurements in the fork's PRs, CPU time by about 10%.
