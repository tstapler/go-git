//go:build !unix

package filesystem

func mapFileReadOnly(uintptr, int64) ([]byte, error) { return nil, errMmapUnsupported }

func unmapBytes([]byte) error { return nil }
