// Package filesystem is a storage backend base on filesystems
package filesystem

import (
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/storage/filesystem/dotgit"

	"github.com/go-git/go-billy/v5"
)

// Storage is an implementation of git.Storer that stores data on disk in the
// standard git format (this is, the .git directory). Zero values of this type
// are not safe to use, see the NewStorage function below.
type Storage struct {
	fs  billy.Filesystem
	dir *dotgit.DotGit

	ObjectStorage
	ReferenceStorage
	IndexStorage
	ShallowStorage
	ConfigStorage
	ModuleStorage
}

// Options holds configuration for the storage.
type Options struct {
	// ExclusiveAccess means that the filesystem is not modified externally
	// while the repo is open.
	ExclusiveAccess bool
	// KeepDescriptors makes the file descriptors to be reused but they will
	// need to be manually closed calling Close().
	KeepDescriptors bool
	// MaxOpenDescriptors is the max number of file descriptors to keep
	// open. If KeepDescriptors is true, all file descriptors will remain open.
	MaxOpenDescriptors int
	// MaxIdlePackHandles bounds the open packfile handles kept idle between object reads when
	// neither KeepDescriptors nor MaxOpenDescriptors is set. 0 selects a default of 32; a negative
	// value disables the pool and reopens the pack for every read. Handles are closed by Close
	// (or by the garbage collector if the storage is dropped without Close).
	MaxIdlePackHandles int
	// MaxMappedPackBytes enables memory-mapped pack reads (Unix only) and caps the total mapped
	// size; 0 or negative leaves it off and reads packs with ReadAt. It is opt-in because it did not
	// pay off everywhere: on macOS reading all blobs and patch statistics used about 13-20% less CPU
	// but a tree diff used 2.5x more (page faults on scattered small reads); see bench/README.md.
	// Packs that cannot be mapped (not an OS file, over the cap) fall back to ReadAt. Pack files
	// deleted by gc or repack stay mapped until their last reader closes or Reindex or Close runs.
	MaxMappedPackBytes int64
	// OnMappedPackBytes, when set, is called with the mapped byte total and mapping count whenever
	// they change, for metrics.
	OnMappedPackBytes func(bytes int64, count int)
	// OnOpenPackHandles, when set, is called with the number of open pooled pack handles each time
	// it changes, for metrics. It must be fast and must not call back into the storage.
	OnOpenPackHandles func(open int64)
	// LargeObjectThreshold maximum object size (in bytes) that will be read in to memory.
	// If left unset or set to 0 there is no limit
	LargeObjectThreshold int64
	// AlternatesFS provides the billy filesystem to be used for Git Alternates.
	// If none is provided, it falls back to using the underlying instance used for
	// DotGit.
	AlternatesFS billy.Filesystem
}

// NewStorage returns a new Storage backed by a given `fs.Filesystem` and cache.
func NewStorage(fs billy.Filesystem, cache cache.Object) *Storage {
	return NewStorageWithOptions(fs, cache, Options{})
}

// NewStorageWithOptions returns a new Storage with extra options,
// backed by a given `fs.Filesystem` and cache.
func NewStorageWithOptions(fs billy.Filesystem, cache cache.Object, ops Options) *Storage {
	dirOps := dotgit.Options{
		ExclusiveAccess: ops.ExclusiveAccess,
		AlternatesFS:    ops.AlternatesFS,
	}
	dir := dotgit.NewWithOptions(fs, dirOps)

	return &Storage{
		fs:  fs,
		dir: dir,

		ObjectStorage:    *NewObjectStorageWithOptions(dir, cache, ops),
		ReferenceStorage: ReferenceStorage{dir: dir},
		IndexStorage:     IndexStorage{dir: dir},
		ShallowStorage:   ShallowStorage{dir: dir},
		ConfigStorage:    ConfigStorage{dir: dir},
		ModuleStorage:    ModuleStorage{dir: dir},
	}
}

// Filesystem returns the underlying filesystem
func (s *Storage) Filesystem() billy.Filesystem {
	return s.fs
}

// Init initializes .git directory
func (s *Storage) Init() error {
	return s.dir.Initialize()
}

func (s *Storage) AddAlternate(remote string) error {
	return s.dir.AddAlternate(remote)
}
