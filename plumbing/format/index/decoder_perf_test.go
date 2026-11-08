package index

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/utils/binary"
	"github.com/stretchr/testify/require"
)

// legacyReadEntry is the pre-optimisation entry decoder (encoding/binary.Read per
// field, one Entry allocation per entry), kept as the oracle for the fast path.
func legacyReadEntry(d *Decoder, idx *Index) (*Entry, error) {
	e := &Entry{}
	var msec, mnsec, sec, nsec uint32
	var flags uint16
	flow := []interface{}{&sec, &nsec, &msec, &mnsec, &e.Dev, &e.Inode, &e.Mode, &e.UID, &e.GID, &e.Size, &e.Hash, &flags}
	if err := binary.Read(d.r, flow...); err != nil {
		return nil, err
	}
	read := entryHeaderLength
	if sec != 0 || nsec != 0 {
		e.CreatedAt = time.Unix(int64(sec), int64(nsec))
	}
	if msec != 0 || mnsec != 0 {
		e.ModifiedAt = time.Unix(int64(msec), int64(mnsec))
	}
	e.Stage = Stage(flags>>12) & 0x3
	if flags&entryExtended != 0 {
		extended, err := binary.ReadUint16(d.r)
		if err != nil {
			return nil, err
		}
		read += 2
		e.IntentToAdd = extended&intentToAddMask != 0
		e.SkipWorktree = extended&skipWorkTreeMask != 0
	}
	nameConsumed, err := d.readEntryName(idx, e, flags)
	if err != nil {
		return nil, err
	}
	return e, d.padEntry(idx, e, read, nameConsumed)
}

func decodeLegacy(t testing.TB, data []byte) *Index {
	t.Helper()
	d := NewDecoder(bytes.NewReader(data))
	idx := &Index{}
	var err error
	idx.Version, err = validateHeader(d.r)
	require.NoError(t, err)
	n, err := binary.ReadUint32(d.r)
	require.NoError(t, err)
	for i := 0; i < int(n); i++ {
		e, err := legacyReadEntry(d, idx)
		require.NoError(t, err)
		d.lastEntry = e
		idx.Entries = append(idx.Entries, e)
	}
	require.NoError(t, d.readExtensions(idx))
	return idx
}

func randomIndex(rng *rand.Rand, version uint32, n int) *Index {
	idx := &Index{Version: version}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("dir%d/sub%d/file-%d.go", i%7, i%3, i)
		switch i % 11 {
		case 3:
			name = strings.Repeat("long/", 900) + fmt.Sprint(i) // beyond the 12-bit name length
		case 5:
			name = fmt.Sprint(i)
		}
		e := &Entry{
			Name: name, Mode: filemode.Regular, Size: rng.Uint32(),
			Dev: rng.Uint32(), Inode: rng.Uint32(), UID: rng.Uint32(), GID: rng.Uint32(),
			CreatedAt:  time.Unix(int64(rng.Intn(1<<30)), int64(rng.Intn(1e9))),
			ModifiedAt: time.Unix(int64(rng.Intn(1<<30)), int64(rng.Intn(1e9))),
		}
		if i%13 == 0 {
			e.ModifiedAt = time.Time{}
		}
		rng.Read(e.Hash[:])
		if version >= 3 && i%17 == 0 {
			e.IntentToAdd, e.SkipWorktree = true, i%2 == 0
		}
		idx.Entries = append(idx.Entries, e)
	}
	return idx
}

func encodeIndex(t testing.TB, idx *Index) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, NewEncoder(&buf).Encode(idx))
	return buf.Bytes()
}

func TestDecodeFastPath_ShouldMatchLegacyDecoder(t *testing.T) {
	t.Parallel()
	for _, version := range []uint32{2, 3, 4} {
		for _, n := range []int{0, 1, 255, 256, 257, 1000} {
			t.Run(fmt.Sprintf("v%d_n%d", version, n), func(t *testing.T) {
				t.Parallel()
				rng := rand.New(rand.NewSource(int64(version)*1000 + int64(n)))
				data := encodeIndex(t, randomIndex(rng, version, n))
				got := &Index{}
				require.NoError(t, NewDecoder(bytes.NewReader(data)).Decode(got))
				want := decodeLegacy(t, data)
				require.Len(t, got.Entries, len(want.Entries))
				for i := range want.Entries {
					require.Equal(t, *want.Entries[i], *got.Entries[i], "entry %d", i)
				}
			})
		}
	}
}

func TestDecodeFastPath_ShouldRejectTruncatedHeader(t *testing.T) {
	t.Parallel()
	data := encodeIndex(t, randomIndex(rand.New(rand.NewSource(1)), 2, 3))
	for _, cut := range []int{12 + 10, 12 + entryHeaderLength - 1, 12 + entryHeaderLength + 2} {
		err := NewDecoder(bytes.NewReader(data[:cut])).Decode(&Index{})
		require.Error(t, err, "cut=%d", cut)
		require.True(t, errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF), "cut=%d: %v", cut, err)
	}
}

// TestDecode_ShouldAllocateAtMostThreePerEntry gates the fix for per-field
// allocations in binary.Read: the legacy decoder spent 15.2 allocations per entry
// (BenchmarkDecodeLegacy), the fast path 1 (the name string) plus 2 for each
// over-long name (the 1-in-11 entries of randomIndex that take the ReadUntil path).
func TestDecode_ShouldAllocateAtMostThreePerEntry(t *testing.T) {
	const n = 2000
	data := encodeIndex(t, randomIndex(rand.New(rand.NewSource(7)), 2, n))
	allocs := testing.AllocsPerRun(5, func() {
		if err := NewDecoder(bytes.NewReader(data)).Decode(&Index{}); err != nil {
			t.Fatal(err)
		}
	})
	if per := allocs / n; per > 3 {
		t.Fatalf("%.1f allocs/entry, want <= 3", per)
	}
}

func BenchmarkDecode(b *testing.B) {
	data := encodeIndex(b, randomIndex(rand.New(rand.NewSource(7)), 2, 10000))
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		if err := NewDecoder(bytes.NewReader(data)).Decode(&Index{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeLegacy(b *testing.B) {
	data := encodeIndex(b, randomIndex(rand.New(rand.NewSource(7)), 2, 10000))
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		decodeLegacy(b, data)
	}
}
