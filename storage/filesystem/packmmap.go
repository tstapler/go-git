package filesystem

import (
	"errors"
	"io"
	"os"
	"runtime/debug"
	"sync"

	billy "github.com/go-git/go-billy/v5"
)

// errMmapUnsupported means the file or platform cannot be mapped; callers fall back to ReadAt.
var errMmapUnsupported = errors.New("memory mapping unsupported")

// errMappedRead is returned when reading a mapping faults (the file was truncated underneath it).
var errMappedRead = errors.New("pack file changed while mapped")

// packMapping is one read-only mapping of a pack file, shared by every handle opened on it.
type packMapping struct {
	path   string
	data   []byte
	refs   int
	dead   bool // evicted or shut down: unmap once refs drops to zero
	lruSeq uint64
}

// packMapRegistry maps pack files and bounds the total mapped bytes. A mapping is reference
// counted by the handles reading it and is unmapped only when nobody does, so a read can never
// touch an unmapped region. Deleting or replacing a pack file (git gc, repack) is safe on Unix:
// the mapping keeps the old inode alive until the last handle closes or the mapping is evicted.
type packMapRegistry struct {
	mu       sync.Mutex
	maps     map[string]*packMapping
	bytes    int64
	max      int64
	seq      uint64
	shut     bool
	onChange func(bytes int64, count int)
}

func newPackMapRegistry(max int64, onChange func(bytes int64, count int)) *packMapRegistry {
	return &packMapRegistry{maps: make(map[string]*packMapping), max: max, onChange: onChange}
}

func (r *packMapRegistry) notify() {
	if r.onChange != nil {
		r.onChange(r.bytes, len(r.maps))
	}
}

// open returns a handle on the mapped pack at path, mapping it from f when it is not mapped yet.
// opener is called only when a new mapping is needed; its file is closed after mapping.
func (r *packMapRegistry) open(path string, opener func() (*os.File, error)) (billy.File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shut {
		return nil, errMmapUnsupported
	}
	m := r.maps[path]
	if m == nil {
		osf, err := opener()
		if err != nil {
			return nil, errMmapUnsupported
		}
		defer osf.Close()
		st, err := osf.Stat()
		if err != nil {
			return nil, err
		}
		size := st.Size()
		if size <= 0 || size > r.max {
			return nil, errMmapUnsupported
		}
		r.evictLocked(size)
		if r.bytes+size > r.max {
			return nil, errMmapUnsupported // everything left is in use
		}
		data, err := mapFileReadOnly(osf.Fd(), size)
		if err != nil {
			return nil, err
		}
		m = &packMapping{path: path, data: data}
		r.maps[path] = m
		r.bytes += size
		r.notify()
	}
	r.seq++
	m.lruSeq = r.seq
	m.refs++
	return &mmapFile{reg: r, m: m}, nil
}

// evictLocked unmaps unused mappings, least recently used first, until need more bytes fit.
func (r *packMapRegistry) evictLocked(need int64) {
	for r.bytes+need > r.max {
		var victim *packMapping
		for _, m := range r.maps {
			if m.refs == 0 && (victim == nil || m.lruSeq < victim.lruSeq) {
				victim = m
			}
		}
		if victim == nil {
			return
		}
		r.unmapLocked(victim)
	}
}

func (r *packMapRegistry) unmapLocked(m *packMapping) {
	delete(r.maps, m.path)
	r.bytes -= int64(len(m.data))
	_ = unmapBytes(m.data)
	m.data = nil
	r.notify()
}

func (r *packMapRegistry) release(m *packMapping) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m.refs--
	if m.refs == 0 && (m.dead || r.shut) {
		r.unmapLocked(m)
	}
}

// drop unmaps every unused mapping now and the in-use ones when their last handle closes
// (the pack set changed: Reindex).
func (r *packMapRegistry) drop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.maps {
		if m.refs == 0 {
			r.unmapLocked(m)
		} else {
			m.dead = true
		}
	}
}

// shutdown is drop plus refusing new mappings (Close).
func (r *packMapRegistry) shutdown() {
	r.mu.Lock()
	r.shut = true
	r.mu.Unlock()
	r.drop()
}

// mappedBytes reports the currently mapped bytes and mappings.
func (r *packMapRegistry) mappedBytes() (int64, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bytes, len(r.maps)
}

// mmapFile is a billy.File over a shared read-only mapping. Reads copy out of the mapping, so
// there are no read syscalls; a truncated file turns into errMappedRead instead of a SIGBUS.
type mmapFile struct {
	reg    *packMapRegistry
	m      *packMapping
	off    int64
	closed bool
}

func (f *mmapFile) Name() string { return f.m.path }

func (f *mmapFile) Write([]byte) (int, error) { return 0, errors.New("mmapFile is read-only") }

func (f *mmapFile) Truncate(int64) error { return errors.New("mmapFile is read-only") }

func (f *mmapFile) Lock() error   { return nil }
func (f *mmapFile) Unlock() error { return nil }

func (f *mmapFile) Close() error {
	if f.closed {
		return os.ErrClosed
	}
	f.closed = true
	f.reg.release(f.m)
	return nil
}

func (f *mmapFile) Read(p []byte) (int, error) {
	n, err := f.ReadAt(p, f.off)
	f.off += int64(n)
	return n, err
}

func (f *mmapFile) ReadAt(p []byte, off int64) (n int, err error) {
	if f.closed {
		return 0, os.ErrClosed
	}
	data := f.m.data
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if off >= int64(len(data)) {
		return 0, io.EOF
	}
	n, err = guardedCopy(p, data[off:])
	if err == nil && n < len(p) {
		err = io.EOF
	}
	return n, err
}

func (f *mmapFile) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.off + offset
	case io.SeekEnd:
		abs = int64(len(f.m.data)) + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("negative position")
	}
	f.off = abs
	return abs, nil
}

// guardedCopy copies from a mapping, converting a fault (the underlying file was truncated) into
// an error. SetPanicOnFault only affects the calling goroutine and is restored on return.
func guardedCopy(dst, src []byte) (n int, err error) {
	old := debug.SetPanicOnFault(true)
	defer func() {
		debug.SetPanicOnFault(old)
		if r := recover(); r != nil {
			n, err = 0, errMappedRead
		}
	}()
	return copy(dst, src), nil
}
