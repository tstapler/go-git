package object

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/filemode"
)

// familyContent is ten lines of family-specific text with the first `edits` lines replaced
// (so edits=3 is roughly 70% similar); kind selects empty, binary and CRLF variants.
func familyContent(fam, edits, kind int) string {
	var sb strings.Builder
	for i := 0; i < 10; i++ {
		if i < edits {
			fmt.Fprintf(&sb, "changed %d %d\n", fam, i)
		} else {
			fmt.Fprintf(&sb, "family %d line %d with some padding text\n", fam, i)
		}
	}
	s := sb.String()
	switch kind {
	case 1:
		return ""
	case 2:
		return "\x00\x01bin" + s
	case 3:
		return strings.ReplaceAll(s, "\n", "\r\n")
	}
	return s
}

// broadRenameFixture builds families of 0-3 identical deleted sources and 0-3 added destinations
// that are exact copies or 1- or 3-line edits, over a small path pool so basenames and directories
// collide, with executable files, symlinks and empty/binary/CRLF contents mixed in. Edits stay
// clear of the similarity thresholds, where git's byte-weighted estimate and ours legitimately differ.
func broadRenameFixture(rng *rand.Rand) (before, after []parityFile) {
	dirs := []string{"a", "b", "c", "a/b", "a/c/d", "", "d/e"}
	bases := []string{"x.go", "y.go", "z.go", "w", "q.txt"}
	used := map[string]bool{}
	path := func() string {
		for {
			b := bases[rng.Intn(len(bases))]
			if rng.Intn(4) == 0 {
				b = fmt.Sprintf("u%d.go", rng.Intn(6))
			}
			p := b
			if d := dirs[rng.Intn(len(dirs))]; d != "" {
				p = d + "/" + b
			}
			if !used[p] {
				used[p] = true
				return p
			}
		}
	}
	mode := func() filemode.FileMode {
		switch r := rng.Intn(10); {
		case r == 0:
			return filemode.Executable
		case r == 1:
			return filemode.Symlink
		}
		return filemode.Regular
	}
	for fam := 0; fam < 1+rng.Intn(6); fam++ {
		kind := 0
		if rng.Intn(8) == 0 {
			kind = 1 + rng.Intn(3)
		}
		file := func(edits int) parityFile {
			m := mode()
			content := familyContent(fam, edits, kind)
			if m == filemode.Symlink {
				content = fmt.Sprintf("tgt%d", fam)
			}
			return parityFile{path(), content, m}
		}
		for n := rng.Intn(4); n > 0; n-- {
			before = append(before, file(0))
		}
		for n := rng.Intn(4); n > 0; n-- {
			after = append(after, file([]int{0, 0, 1, 3}[rng.Intn(4)]))
		}
	}
	return before, after
}

// Pairings must equal `git diff -M -l<limit>` over broad randomized fixtures: identical-content
// groups next to an edited copy, symlink and unrelated sources, shared basenames, several scores
// and limits. Covers the 4-slot candidate tie order and the stricter basename pre-pass score.
func TestDetectRenames_BroadRandomFixtures_MatchGitCLI(t *testing.T) {
	requireGitOracle(t)
	limits := []int{0, 1, 2, 3, 4, 5, 6, 1000}
	scores := []int{30, 50, 60}
	for seed := int64(1000); seed < 1150; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewSource(seed))
			before, after := broadRenameFixture(rng)
			if len(before) == 0 || len(after) == 0 {
				t.Skip("empty side")
			}
			limit, score := limits[rng.Intn(len(limits))], scores[rng.Intn(len(scores))]
			// git treats -l0 as "default", ours as unlimited; compare only where they agree.
			gitLimit := limit
			if limit == 0 {
				gitLimit = 32767
			}
			want := gitRenames(t, before, after, score, gitLimit)
			got := goGitRenames(t, before, after, score, limit)
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("limit %d score %d: renames differ from git\n before: %v\n after: %v\n git: %v\n got: %v", limit, score, before, after, want, got)
			}
		})
	}
}
