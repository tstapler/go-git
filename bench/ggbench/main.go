// Command ggbench times go-git operations for the cross-library comparison in bench/README.md.
//
//	go run ./bench/ggbench <repo> <base-rev> <head-rev>
//
// GGBENCH_KEEPFD=1 opens with filesystem.Options{KeepDescriptors: true}.
// GGBENCH_ONLY=<op> restricts it to one operation and GGBENCH_CPUPROFILE=<file> writes a CPU profile.
// It prints one "op<TAB>milliseconds<TAB>result-count" line per operation (best of 3).
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime/pprof"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

func best(f func() int) (time.Duration, int) {
	var d time.Duration
	var n int
	for i := 0; i < 3; i++ {
		s := time.Now()
		n = f()
		if e := time.Since(s); i == 0 || e < d {
			d = e
		}
	}
	return d, n
}

// openRepo opens with go-git's defaults, or with KeepDescriptors when GGBENCH_KEEPFD=1: by default
// every packed-object read reopens its packfile, which is what stapler-squad avoids with KeepDescriptors.
func openRepo(path string) *git.Repository {
	if os.Getenv("GGBENCH_KEEPFD") != "1" {
		return must(git.PlainOpen(path))
	}
	dot := osfs.New(path + "/.git")
	st := filesystem.NewStorageWithOptions(dot, cache.NewObjectLRUDefault(), filesystem.Options{KeepDescriptors: true})
	return must(git.Open(st, osfs.New(path)))
}

func must[T any](v T, err error) T {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return v
}

func main() {
	repoPath, baseRev, headRev := os.Args[1], os.Args[2], os.Args[3]
	if path := os.Getenv("GGBENCH_CPUPROFILE"); path != "" {
		f := must(os.Create(path))
		_ = pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	repo := openRepo(repoPath)
	bt := must(must(repo.CommitObject(*must(repo.ResolveRevision(plumbing.Revision(baseRev))))).Tree())
	hc := must(repo.CommitObject(*must(repo.ResolveRevision(plumbing.Revision(headRev)))))
	ht := must(hc.Tree())
	ctx := context.Background()
	only := os.Getenv("GGBENCH_ONLY")
	report := func(op string, f func() int) {
		if only != "" && only != op {
			return
		}
		d, n := best(f)
		fmt.Printf("%s\t%.1f\t%d\n", op, float64(d.Microseconds())/1000, n)
	}
	opts := &object.DiffTreeOptions{DetectRenames: true, RenameScore: 50, RenameLimit: object.DefaultRenameLimit}

	report("treediff", func() int { return len(must(object.DiffTreeWithOptions(ctx, bt, ht, nil))) })
	report("rename-detect", func() int {
		n := 0
		for _, c := range must(object.DiffTreeWithOptions(ctx, bt, ht, opts)) {
			if c.From.Name != "" && c.To.Name != "" && c.From.Name != c.To.Name {
				n++
			}
		}
		return n
	})
	report("detect+patch-stats", func() int {
		ch := must(object.DiffTreeWithOptions(ctx, bt, ht, opts))
		return len(must(ch.PatchContext(ctx)).Stats())
	})
	report("index-read", func() int { return len(must(repo.Storer.Index()).Entries) })
	report("log-walk", func() int {
		n := 0
		it := must(repo.Log(&git.LogOptions{From: hc.Hash}))
		_ = it.ForEach(func(*object.Commit) error { n++; return nil })
		return n
	})
	report("blob-read-all", func() int {
		var total int64
		_ = ht.Files().ForEach(func(f *object.File) error {
			r := must(f.Reader())
			n, _ := io.Copy(io.Discard, r)
			r.Close()
			total += n
			return nil
		})
		return int(total)
	})
	report("status", func() int {
		wt := must(repo.Worktree())
		return len(must(wt.Status()))
	})
}
