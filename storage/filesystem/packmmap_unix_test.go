//go:build unix

package filesystem

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/storage/filesystem/dotgit"

	fixtures "github.com/go-git/go-git-fixtures/v4"
)

func writeTemp(t *testing.T, size int) (string, []byte) {
	t.Helper()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*7 + i/251)
	}
	path := filepath.Join(t.TempDir(), "f.pack")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, data
}

func opener(path string) func() (*os.File, error) {
	return func() (*os.File, error) { return os.Open(path) }
}

// Reads through the mapping must equal reads through the file.
func TestMmapFile_ShouldReadByteForByteLikeTheFile(t *testing.T) {
	path, want := writeTemp(t, 200_000)
	reg := newPackMapRegistry(1<<30, nil)
	f, err := reg.open(path, opener(path))
	if err != nil {
		t.Fatal(err)
	}
	all, err := io.ReadAll(f)
	if err != nil || !bytes.Equal(all, want) {
		t.Fatalf("sequential read differs (err %v)", err)
	}
	buf := make([]byte, 4096)
	for _, off := range []int64{0, 1, 4095, 4096, 100_000, int64(len(want)) - 4096} {
		n, err := f.ReadAt(buf, off)
		if err != nil || !bytes.Equal(buf[:n], want[off:off+int64(n)]) {
			t.Fatalf("ReadAt %d differs (err %v)", off, err)
		}
	}
	if n, err := f.ReadAt(buf, int64(len(want))-10); n != 10 || err != io.EOF {
		t.Fatalf("short read at end: n=%d err=%v", n, err)
	}
	if _, err := f.Seek(-5, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.Read(buf); n != 5 {
		t.Fatalf("seek from end then read gave %d bytes", n)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadAt(buf, 0); err == nil {
		t.Fatal("read after close must fail")
	}
}

func TestPackMapRegistry_ShouldBoundMappedBytesAndEvictLeastRecentlyUsed(t *testing.T) {
	var lastBytes int64
	var lastCount int
	reg := newPackMapRegistry(250_000, func(b int64, c int) { lastBytes, lastCount = b, c })
	p1, _ := writeTemp(t, 100_000)
	p2, _ := writeTemp(t, 100_000)
	p3, _ := writeTemp(t, 100_000)
	huge, _ := writeTemp(t, 300_000)

	open := func(p string) io.Closer {
		f, err := reg.open(p, opener(p))
		if err != nil {
			t.Fatalf("open %s: %v", p, err)
		}
		return f
	}
	open(p1).Close()
	open(p2).Close()
	if b, c := reg.mappedBytes(); b != 200_000 || c != 2 {
		t.Fatalf("mapped %d bytes in %d maps", b, c)
	}
	open(p3).Close() // evicts p1 (least recently used), keeping the total under the cap
	if b, c := reg.mappedBytes(); b != 200_000 || c != 2 || lastBytes != 200_000 || lastCount != 2 {
		t.Fatalf("after eviction mapped %d bytes in %d maps (hook %d/%d)", b, c, lastBytes, lastCount)
	}
	if _, ok := reg.maps[p1]; ok {
		t.Fatal("least recently used pack was not evicted")
	}
	if _, err := reg.open(huge, opener(huge)); err == nil {
		t.Fatal("a pack over the cap must not be mapped")
	}

	// A mapping in use is never evicted, so a new pack that does not fit is refused.
	held2 := open(p2)
	held3 := open(p3)
	if _, err := reg.open(p1, opener(p1)); err == nil {
		t.Fatal("opening past the cap with everything in use must fail, not unmap a live mapping")
	}
	held2.Close()
	held3.Close()
	reg.shutdown()
	if b, c := reg.mappedBytes(); b != 0 || c != 0 {
		t.Fatalf("shutdown left %d bytes in %d maps", b, c)
	}
	if _, err := reg.open(p1, opener(p1)); err == nil {
		t.Fatal("a shut down registry must refuse new mappings")
	}
}

// git gc and repack delete pack files while readers use them: reads must keep returning the old
// bytes, and the mapping must go away only after the last reader.
func TestPackMapRegistry_ShouldSurviveDeletionOfTheMappedFile(t *testing.T) {
	path, want := writeTemp(t, 300_000)
	reg := newPackMapRegistry(1<<30, nil)
	f, err := reg.open(path, opener(path))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			h, err := reg.open(path, opener(path))
			if err != nil {
				errs <- err
				return
			}
			defer h.Close()
			buf := make([]byte, 1024)
			for i := 0; i < 2000; i++ {
				off := int64((i*131 + w*17) % (len(want) - 1024))
				n, err := h.ReadAt(buf, off)
				if err != nil || !bytes.Equal(buf[:n], want[off:off+int64(n)]) {
					errs <- err
					return
				}
				if i == 500 && w == 0 {
					_ = os.Remove(path) // gc deletes the pack mid-read
					reg.drop()          // Reindex after the pack set changed
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("read failed while the pack was deleted: %v", err)
	}
	buf := make([]byte, 16)
	if _, err := f.ReadAt(buf, 1000); err != nil || !bytes.Equal(buf, want[1000:1016]) {
		t.Fatalf("held handle must still read the deleted pack (err %v)", err)
	}
	if b, _ := reg.mappedBytes(); b == 0 {
		t.Fatal("mapping released while a handle was still open")
	}
	f.Close()
	if b, c := reg.mappedBytes(); b != 0 || c != 0 {
		t.Fatalf("last close after drop must unmap, left %d bytes in %d maps", b, c)
	}
}

// A file truncated under a live mapping would SIGBUS; the read helper must turn it into an error.
func TestMmapFile_ShouldReturnErrorInsteadOfFaultingWhenFileIsTruncated(t *testing.T) {
	path, _ := writeTemp(t, 3*4096)
	reg := newPackMapRegistry(1<<30, nil)
	f, err := reg.open(path, opener(path))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Truncate(path, 4096); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	// Whether the kernel faults on a truncated page depends on the platform and page cache: the
	// requirement is only that the process survives and any failure is errMappedRead.
	if _, err := f.ReadAt(buf, 2*4096); err != nil && err != errMappedRead {
		t.Fatalf("read of a truncated region = %v, want nil or errMappedRead", err)
	}
	if n, err := f.ReadAt(buf, 0); err != nil || n != 4096 {
		t.Fatalf("read of the intact region failed: n=%d err=%v", n, err)
	}
}

// With mapping enabled the storage must return the same objects as without, and release its mappings.
func TestObjectStorage_ShouldReadSameObjectsWithMappingEnabled(t *testing.T) {
	dotGit := fixtures.Basic().ByTag(".git").One().DotGit()
	hash := plumbing.NewHash("6ecf0ef2c2dffb796033e5a02219af86ec6584e5")
	plain := NewObjectStorageWithOptions(dotgit.New(dotGit), cache.NewObjectLRUDefault(), Options{})
	var mappedHook int64
	mapped := NewObjectStorageWithOptions(dotgit.New(osfs.New(fixtureRoot(dotGit))), cache.NewObjectLRUDefault(),
		Options{MaxMappedPackBytes: 1 << 30, OnMappedPackBytes: func(b int64, _ int) { mappedHook = b }})

	want, err := plain.EncodedObject(plumbing.AnyObject, hash)
	if err != nil {
		t.Fatal(err)
	}
	got, err := mapped.EncodedObject(plumbing.AnyObject, hash)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hash() != want.Hash() || got.Size() != want.Size() || got.Type() != want.Type() {
		t.Fatal("mapped storage returned a different object")
	}
	if b, c := mapped.MappedPacks(); b == 0 || c == 0 || mappedHook != b {
		t.Fatalf("expected the pack to be mapped, got %d bytes in %d maps (hook %d)", b, c, mappedHook)
	}
	if err := mapped.Close(); err != nil {
		t.Fatal(err)
	}
	if b, c := mapped.MappedPacks(); b != 0 || c != 0 {
		t.Fatalf("Close left %d bytes in %d maps", b, c)
	}
}

func fixtureRoot(fs interface{ Root() string }) string { return fs.Root() }
