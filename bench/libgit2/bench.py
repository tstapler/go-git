#!/usr/bin/env python3
# /// script
# dependencies = ["pygit2"]
# ///
"""Compare go-git (this fork), libgit2 (pygit2) and the git CLI on the same repos.

    uv run bench/libgit2/bench.py --ggbench /tmp/ggbench [--squad-clone /tmp/ssq-clone] [--only n=1000]

libgit2 is a PERFORMANCE comparator only: its similarity algorithm, thresholds and basename handling
differ from git's, so result counts can differ (printed next to every time). Parity is checked
against the git CLI by the oracle tests in plumbing/object, not here.
"""
import argparse, os, subprocess, sys, tempfile, time
import pygit2

ENV = dict(os.environ, GIT_AUTHOR_NAME="t", GIT_AUTHOR_EMAIL="t@t", GIT_COMMITTER_NAME="t",
           GIT_COMMITTER_EMAIL="t@t", GIT_OPTIONAL_LOCKS="0")

def git(repo, *args):
    return subprocess.run(["git", "-C", repo, *args], env=ENV, check=True, capture_output=True).stdout

def body(i, lines):
    return "".join(f"file {i} line {l} some padding text to make lines longer\n" for l in range(lines))

def write(repo, rel, content):
    path = os.path.join(repo, rel)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write(content)

def moved(n, lines, edit):
    def build(repo):
        git(repo, "init", "-q")
        for i in range(n):
            write(repo, f"a/f{i:05d}.txt", body(i, lines))
        git(repo, "add", "-A"); git(repo, "commit", "-qm", "a")
        base = git(repo, "rev-parse", "HEAD").decode().strip()
        git(repo, "rm", "-rq", "a")
        for i in range(n):
            write(repo, f"b/g{i:05d}.txt", body(i, lines) + ("edit\n" if edit else ""))
        git(repo, "add", "-A"); git(repo, "commit", "-qm", "b")
        return base, git(repo, "rev-parse", "HEAD").decode().strip()
    return build

FIXTURES = {
    "moved+edited n=120": moved(120, 10, True),
    "moved+edited n=400": moved(400, 10, True),
    "moved+edited n=1000": moved(1000, 10, True),
    "moved+edited n=2000": moved(2000, 10, True),
    "moved+edited n=400 2000-line files": moved(400, 2000, True),
    "identical moved n=2000": moved(2000, 1, False),
    "identical moved n=8000": moved(8000, 1, False),
}

def timed(f, reps=3):
    best, res = None, None
    for _ in range(reps):
        s = time.perf_counter(); res = f(); e = (time.perf_counter() - s) * 1000
        best = e if best is None or e < best else best
    return best, res

def cli_ops(repo, base, head):
    def run(*a):
        return lambda: len(git(repo, *a).splitlines())
    return {
        "treediff": run("diff", "--name-status", "--no-renames", base, head),
        "rename-detect": run("diff", "--name-status", "-M50%", "-l1000", base, head),
        "detect+patch-stats": run("diff", "--numstat", "-M50%", "-l1000", base, head),
        "index-read": run("ls-files", "-s"),
        "log-walk": run("rev-list", head),
        "blob-read-all": lambda: len(git(repo, "archive", "--format=tar", head)),
        "status": run("status", "--porcelain"),
    }

def l2_ops(repo, base, head):
    r = pygit2.Repository(repo)
    bt, ht = r.revparse_single(base).tree, r.revparse_single(head).tree
    def renames(d):
        d.find_similar(flags=pygit2.GIT_DIFF_FIND_RENAMES, rename_threshold=50, rename_limit=1000)
        return sum(1 for x in d.deltas if x.status == pygit2.GIT_DELTA_RENAMED)
    def walk_blobs():
        total = 0
        def rec(t):
            nonlocal total
            for e in t:
                if e.filemode == pygit2.GIT_FILEMODE_COMMIT:
                    continue  # submodule
                o = r[e.id]
                if isinstance(o, pygit2.Tree): rec(o)
                elif isinstance(o, pygit2.Blob): total += len(o.data)
        rec(ht); return total
    return {
        "treediff": lambda: len(list(r.diff(bt, ht).deltas)),
        "rename-detect": lambda: renames(r.diff(bt, ht)),
        "detect+patch-stats": lambda: (lambda d: (d.find_similar(flags=pygit2.GIT_DIFF_FIND_RENAMES, rename_threshold=50, rename_limit=1000), d.stats.files_changed)[1])(r.diff(bt, ht)),
        "index-read": lambda: (r.index.read(), len(r.index))[1],
        "log-walk": lambda: len(list(r.walk(r.revparse_single(head).id))),
        "blob-read-all": walk_blobs,
        "status": lambda: len(r.status()),
    }

def go_ops(ggbench, repo, base, head, keepfd=False):
    env = dict(ENV, GGBENCH_KEEPFD="1" if keepfd else "0")
    out = subprocess.run([ggbench, repo, base, head], env=env, check=True, capture_output=True, text=True).stdout
    return {l.split("\t")[0]: (float(l.split("\t")[1]), int(l.split("\t")[2])) for l in out.splitlines()}  # a 4th cpu=<ms> column is for manual comparison

def report(title, repo, base, head, ggbench):
    spawn, _ = timed(lambda: git(repo, "--version"))
    cli = {k: timed(f) for k, f in cli_ops(repo, base, head).items()}
    l2 = {k: timed(f) for k, f in l2_ops(repo, base, head).items()}
    go = go_ops(ggbench, repo, base, head)
    gk = go_ops(ggbench, repo, base, head, keepfd=True)
    print(f"\n### {title}  (git process spawn ~{spawn:.0f} ms)\n")
    print("| op | go-git fork ms (n) | libgit2 ms (n) | git CLI ms (n) | go-git/libgit2 | go-git/git CLI | go-git/git CLI excl. spawn |")
    print("|---|---|---|---|---|---|---|---|")
    for op in cli:
        g, gn = go[op]; k, _ = gk[op]; l, ln = l2[op]; c, cn = cli[op]
        print(f"| {op} | {g:.1f} ({gn}) | {k:.1f} | {l:.1f} ({ln}) | {c:.1f} ({cn}) | {k/l:.2f}x | {k/c:.2f}x | {k/max(c-spawn,1):.2f}x |")
    sys.stdout.flush()

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--ggbench", required=True)
    ap.add_argument("--squad-clone")
    ap.add_argument("--only", default="")
    a = ap.parse_args()
    for name, build in FIXTURES.items():
        if a.only and a.only not in name: continue
        with tempfile.TemporaryDirectory() as d:
            base, head = build(d)
            report(name, d, base, head, a.ggbench)
    if a.squad_clone and (not a.only or "squad" in a.only):
        head = git(a.squad_clone, "rev-parse", "HEAD").decode().strip()
        base = git(a.squad_clone, "rev-parse", "HEAD~200").decode().strip()
        n = len(git(a.squad_clone, "ls-files").splitlines())
        report(f"squad clone ({n} tracked files, HEAD~200..HEAD)", a.squad_clone, base, head, a.ggbench)

if __name__ == "__main__":
    main()
