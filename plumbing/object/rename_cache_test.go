package object

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/storage/memory"
)

func tbFile(tb testing.TB, name, content string, mode filemode.FileMode) *File {
	tb.Helper()
	obj := new(plumbing.MemoryObject)
	obj.SetType(plumbing.BlobObject)
	if _, err := obj.Write([]byte(content)); err != nil {
		tb.Fatal(err)
	}
	return &File{Name: name, Mode: mode, Blob: Blob{Hash: obj.Hash(), Size: obj.Size(), obj: obj}}
}

func tbChange(tb testing.TB, from, to *File) *Change {
	tb.Helper()
	entry := func(f *File) ChangeEntry {
		sto := memory.NewStorage()
		_, _ = sto.SetEncodedObject(f.obj)
		return ChangeEntry{Name: f.Name, Tree: &Tree{s: sto}, TreeEntry: TreeEntry{Name: f.Name[strings.LastIndex(f.Name, "/")+1:], Mode: f.Mode, Hash: f.Hash}}
	}
	c := &Change{}
	if from != nil {
		c.From = entry(from)
	}
	if to != nil {
		c.To = entry(to)
	}
	return c
}

// randomSides builds nSrc deleted and nDst added files drawn from a shared pool of
// lines, so many pairs are similar, with some ties, empty, large and non-regular files.
func randomSides(tb testing.TB, rng *rand.Rand, nSrc, nDst int) (srcs, dsts []*Change) {
	pool := make([]string, 40)
	for i := range pool {
		pool[i] = fmt.Sprintf("pool line %d %d\n", i, rng.Intn(5))
	}
	body := func() string {
		var b strings.Builder
		for n := rng.Intn(30); n >= 0; n-- {
			b.WriteString(pool[rng.Intn(len(pool))])
		}
		return b.String()
	}
	mode := func() filemode.FileMode {
		if rng.Intn(15) == 0 {
			return filemode.Executable
		}
		return filemode.Regular
	}
	for i := 0; i < nSrc; i++ {
		srcs = append(srcs, tbChange(tb, tbFile(tb, fmt.Sprintf("old/d%d/f%d.txt", rng.Intn(3), i), body(), mode()), nil))
	}
	for i := 0; i < nDst; i++ {
		content := body()
		if rng.Intn(10) == 0 {
			content = ""
		}
		dsts = append(dsts, tbChange(tb, nil, tbFile(tb, fmt.Sprintf("new/d%d/g%d.txt", rng.Intn(3), i), content, mode())))
	}
	return srcs, dsts
}

// detectWithBudget runs rename detection over a copy of the changes with a given
// source-index cache budget (0 default, negative none).
func detectWithBudget(t testing.TB, srcs, dsts []*Change, score, budget int) []string {
	t.Helper()
	d := &renameDetector{renameScore: score, indexCacheBudget: budget}
	d.deleted = append([]*Change(nil), srcs...)
	d.added = append([]*Change(nil), dsts...)
	result, err := d.detect()
	if err != nil {
		t.Fatal(err)
	}
	var pairs []string
	for _, c := range result {
		if c.From.Name != "" && c.To.Name != "" {
			pairs = append(pairs, c.From.Name+" "+c.To.Name)
		}
	}
	return pairs
}

// Pairings must not depend on the index cache: with the full budget, a budget that holds only
// a couple of source indexes (the over-budget fallback rebuilds the rest per use) and none.
func TestDetectRenames_ShouldNotDependOnIndexCacheBudget(t *testing.T) {
	// A small index is 256 entries x 8 bytes = 2048 bytes, so 5000 holds two of them.
	for seed := int64(0); seed < 200; seed++ {
		rng := rand.New(rand.NewSource(seed))
		srcs, dsts := randomSides(t, rng, 1+rng.Intn(25), 1+rng.Intn(25))
		for _, score := range []int{30, 60} {
			want := detectWithBudget(t, srcs, dsts, score, -1)
			for _, budget := range []int{0, 5000} {
				if got := detectWithBudget(t, srcs, dsts, score, budget); !reflect.DeepEqual(want, got) {
					t.Fatalf("budget %d seed %d score %d: pairs differ\n none: %v\n got:  %v", budget, seed, score, want, got)
				}
			}
		}
	}
}

func TestSimilarityIndexCache_ShouldCacheOnlyWithinBudget(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	_, dsts := randomSides(t, rng, 1, 10)
	file := func(i int) func() (*File, error) {
		return func() (*File, error) { _, f, err := dsts[i].Files(); return f, err }
	}
	for _, tc := range []struct{ budget, want int }{{0, 0}, {5000, 2}, {maxCachedIndexBytes, 10}} {
		c := newSimilarityIndexCache(tc.budget)
		for i := range dsts {
			if _, err := c.get(i, file(i)); err != nil {
				t.Fatal(err)
			}
		}
		if len(c.indexes) != tc.want {
			t.Errorf("budget %d cached %d indexes, want %d", tc.budget, len(c.indexes), tc.want)
		}
	}
}

// DetectRenames must pair the same files as `git diff -M --name-status` on fixtures whose
// edits are small enough that jgit-style and git spanhash scoring agree.
func TestDetectRenames_MatchesGitCLI(t *testing.T) {
	requireGitOracle(t)
	dir := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return string(out)
	}
	git("init", "-q")
	write := func(path, body string) {
		full := dir + "/" + path
		_ = os.MkdirAll(full[:strings.LastIndex(full, "/")], 0o755)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const n = 12
	body := func(i int) string { return strings.Repeat(fmt.Sprintf("unique content of file %d\n", i), 10) }
	for i := 0; i < n; i++ {
		write(fmt.Sprintf("a/f%02d.txt", i), body(i))
	}
	git("add", "-A")
	git("commit", "-qm", "base")
	base := strings.TrimSpace(git("rev-parse", "HEAD"))
	for i := 0; i < n; i++ {
		_ = os.Remove(fmt.Sprintf("%s/a/f%02d.txt", dir, i))
		write(fmt.Sprintf("b/g%02d.txt", i), body(i)+"edit\n")
	}
	git("add", "-A")
	git("commit", "-qm", "move")
	head := strings.TrimSpace(git("rev-parse", "HEAD"))

	var want []string
	for _, l := range strings.Split(strings.TrimSpace(git("diff", "--name-status", "-M", base, head)), "\n") {
		f := strings.Fields(l)
		if strings.HasPrefix(f[0], "R") {
			want = append(want, f[1]+" "+f[2])
		}
	}
	sort.Strings(want)

	var changes Changes
	for i := 0; i < n; i++ {
		changes = append(changes,
			tbChange(t, tbFile(t, fmt.Sprintf("a/f%02d.txt", i), body(i), filemode.Regular), nil),
			tbChange(t, nil, tbFile(t, fmt.Sprintf("b/g%02d.txt", i), body(i)+"edit\n", filemode.Regular)))
	}
	result, err := DetectRenames(changes, &DiffTreeOptions{RenameScore: 50, RenameLimit: DefaultRenameLimit})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range result {
		if c.From.Name != "" && c.To.Name != "" {
			got = append(got, c.From.Name+" "+c.To.Name)
		}
	}
	sort.Strings(got)
	if !reflect.DeepEqual(want, got) || len(got) != n {
		t.Fatalf("renames differ from git\n git: %v\n got: %v", want, got)
	}
}

func benchSides(b *testing.B, n int) (srcs, dsts []*Change) {
	for i := 0; i < n; i++ {
		body := strings.Repeat(fmt.Sprintf("file %d unique line\n", i), 8)
		srcs = append(srcs, tbChange(b, tbFile(b, fmt.Sprintf("a/f%04d.txt", i), body, filemode.Regular), nil))
		dsts = append(dsts, tbChange(b, nil, tbFile(b, fmt.Sprintf("b/g%04d.txt", i), body+"one edited line\n", filemode.Regular)))
	}
	return srcs, dsts
}

// Run with `&` appended (squad convention): go test ./plumbing/object -run xxx -bench DetectRenames -benchmem &
func BenchmarkDetectRenames(b *testing.B) {
	for _, n := range []int{120, 400, 1000} {
		srcs, dsts := benchSides(b, n)
		for _, tc := range []struct {
			name   string
			budget int
		}{{"cached", 0}, {"uncached", -1}} {
			b.Run(fmt.Sprintf("%s/%d", tc.name, n), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					detectWithBudget(b, srcs, dsts, 60, tc.budget)
				}
			})
		}
	}
}
