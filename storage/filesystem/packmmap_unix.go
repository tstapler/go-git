//go:build unix

package filesystem

import "golang.org/x/sys/unix"

func mapFileReadOnly(fd uintptr, size int64) ([]byte, error) {
	return unix.Mmap(int(fd), 0, int(size), unix.PROT_READ, unix.MAP_SHARED)
}

func unmapBytes(b []byte) error { return unix.Munmap(b) }
