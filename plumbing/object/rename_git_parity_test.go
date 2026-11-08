package object

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/filemode"
)

type parityFile struct {
	path    string
	content string // symlink target for symlinks
	mode    filemode.FileMode
}

// gitRenames commits before then after in a scratch repo and returns the "old new" pairs that
// `git diff --name-status -M<score>% -l<limit>` reports as renames.
func gitRenames(t *testing.T, before, after []parityFile, score, limit int) []string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return string(out)
	}
	write := func(f parityFile) {
		full := filepath.Join(dir, f.path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		switch f.mode {
		case filemode.Symlink:
			if err := os.Symlink(f.content, full); err != nil {
				t.Fatal(err)
			}
		case filemode.Executable:
			if err := os.WriteFile(full, []byte(f.content), 0o755); err != nil {
				t.Fatal(err)
			}
		default:
			if err := os.WriteFile(full, []byte(f.content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	run("init", "-q")
	run("config", "core.symlinks", "true")
	for _, f := range before {
		write(f)
	}
	run("add", "-A")
	run("commit", "-qm", "base")
	base := strings.TrimSpace(run("rev-parse", "HEAD"))
	for _, f := range before {
		_ = os.Remove(filepath.Join(dir, f.path))
	}
	for _, f := range after {
		write(f)
	}
	run("add", "-A")
	run("commit", "-qm", "head")
	var pairs []string
	for _, l := range strings.Split(strings.TrimSpace(run("diff", "--name-status", fmt.Sprintf("-M%d%%", score), fmt.Sprintf("-l%d", limit), base, "HEAD")), "\n") {
		if f := strings.Fields(l); len(f) == 3 && strings.HasPrefix(f[0], "R") {
			pairs = append(pairs, f[1]+" "+f[2])
		}
	}
	sort.Strings(pairs)
	return pairs
}

func goGitRenames(t *testing.T, before, after []parityFile, score, limit int) []string {
	t.Helper()
	var changes Changes
	for _, f := range before {
		changes = append(changes, tbChange(t, tbFile(t, f.path, f.content, f.mode), nil))
	}
	for _, f := range after {
		changes = append(changes, tbChange(t, nil, tbFile(t, f.path, f.content, f.mode)))
	}
	result, err := DetectRenames(changes, &DiffTreeOptions{RenameScore: uint(score), RenameLimit: uint(limit)})
	if err != nil {
		t.Fatal(err)
	}
	var pairs []string
	for _, c := range result {
		if c.From.Name != "" && c.To.Name != "" {
			pairs = append(pairs, c.From.Name+" "+c.To.Name)
		}
	}
	sort.Strings(pairs)
	return pairs
}

func assertParity(t *testing.T, before, after []parityFile, score, limit int) []string {
	t.Helper()
	want := gitRenames(t, before, after, score, limit)
	got := goGitRenames(t, before, after, score, limit)
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("renames differ from git (%d vs %d)\n git: %v\n got: %v", len(want), len(got), want, got)
	}
	return got
}

func lines(i int) string { return strings.Repeat(fmt.Sprintf("unique content of file %d\n", i), 10) }

// Git's exact-rename pass has no limit: only the inexact matrix is gated by it.
func TestDetectRenames_ExactDuplicateGroupsIgnoreLimit_MatchesGitCLI(t *testing.T) {
	for _, tc := range []struct{ n, limit int }{{50, 10}, {1100, 1000}} {
		t.Run(fmt.Sprintf("%dx%d at limit %d", tc.n, tc.n, tc.limit), func(t *testing.T) {
			var before, after []parityFile
			for i := 0; i < tc.n; i++ {
				before = append(before, parityFile{fmt.Sprintf("old/f%04d.py", i), "same\n", filemode.Regular})
				after = append(after, parityFile{fmt.Sprintf("new/f%04d.py", i), "same\n", filemode.Regular})
			}
			if got := assertParity(t, before, after, 50, tc.limit); len(got) != tc.n {
				t.Fatalf("want %d renames, got %d", tc.n, len(got))
			}
		})
	}
}

func TestDetectRenames_FileTypes_MatchesGitCLI(t *testing.T) {
	before := []parityFile{
		{"exe.sh", lines(1), filemode.Executable},
		{"reg_to_exe.txt", lines(2), filemode.Regular},
		{"exe_to_reg.sh", lines(3), filemode.Executable},
		{"lnk1", "some/long/target/path/for/link/number/one", filemode.Symlink},
		{"reg_to_lnk.txt", lines(4), filemode.Regular},
		{"exact_exe.sh", "exact\n", filemode.Executable},
		{"exact_lnk", "tgt", filemode.Symlink},
		{"exact_reg_to_lnk", "other", filemode.Regular},
	}
	after := []parityFile{
		{"n_exe.sh", lines(1) + "edit\n", filemode.Executable},
		{"n_reg_to_exe.txt", lines(2) + "edit\n", filemode.Executable},
		{"n_exe_to_reg.sh", lines(3) + "edit\n", filemode.Regular},
		{"nlnk1", "some/long/target/path/for/link/number/one/x", filemode.Symlink},
		{"n_reg_to_lnk", "unique content of file 4 unique content of file 4", filemode.Symlink},
		{"n_exact.sh", "exact\n", filemode.Regular},
		{"n_exact_lnk", "tgt", filemode.Symlink},
		{"n_exact_reg_to_lnk", "other", filemode.Symlink},
	}
	got := assertParity(t, before, after, 30, 1000)
	if len(got) != 5 {
		t.Fatalf("expected 5 renames (3 inexact regular/executable, exact exe->reg, exact symlink), got %v", got)
	}
}
