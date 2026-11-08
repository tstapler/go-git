// Command ggbench times go-git operations for the cross-library comparison in bench/README.md.
//
//	go run ./bench/ggbench <repo> <base-rev> <head-rev>
//
// GGBENCH_IDLE=<n> opens with that MaxIdlePackHandles.
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
	"strconv"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// cpuTime is the process's user plus system CPU time; unlike wall time it stays comparable on a loaded machine.
func cpuTime() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func best(f func() int) (wall, cpu time.Duration, n int) {
	for i := 0; i < 3; i++ {
		s, c := time.Now(), cpuTime()
		n = f()
		if e, ce := time.Since(s), cpuTime()-c; i == 0 || e < wall {
			wall, cpu = e, ce
		}
	}
	return wall, cpu, n
}

// openRepo opens with go-git's defaults, or with KeepDescriptors when GGBENCH_KEEPFD=1: by default
// every packed-object read reopens its packfile, which is what stapler-squad avoids with KeepDescriptors.
func openRepo(path string) *git.Repository {
	idle, _ := strconv.Atoi(os.Getenv("GGBENCH_IDLE"))
	if os.Getenv("GGBENCH_KEEPFD") != "1" && idle == 0 {
		return must(git.PlainOpen(path))
	}
	dot := osfs.New(path + "/.git")
	st := filesystem.NewStorageWithOptions(dot, cache.NewObjectLRUDefault(), filesystem.Options{
		KeepDescriptors: os.Getenv("GGBENCH_KEEPFD") == "1", MaxIdlePackHandles: idle})
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
		d, cpu, n := best(f)
		fmt.Printf("%s\t%.1f\t%d\tcpu=%.1f\n", op, float64(d.Microseconds())/1000, n, float64(cpu.Microseconds())/1000)
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
