package object

import (
	"bytes"
	"context"

	"github.com/go-git/go-git/v5/utils/merkletrie"
	"github.com/go-git/go-git/v5/utils/merkletrie/noder"
)

// DiffTree compares the content and mode of the blobs found via two
// tree objects.
// DiffTree does not perform rename detection, use DiffTreeWithOptions
// instead to detect renames.
func DiffTree(a, b *Tree) (Changes, error) {
	return DiffTreeContext(context.Background(), a, b)
}

// DiffTreeContext compares the content and mode of the blobs found via two
// tree objects. Provided context must be non-nil.
// An error will be returned if context expires.
func DiffTreeContext(ctx context.Context, a, b *Tree) (Changes, error) {
	return DiffTreeWithOptions(ctx, a, b, nil)
}

// DiffTreeOptions are the configurable options when performing a diff tree.
type DiffTreeOptions struct {
	// DetectRenames is whether the diff tree will use rename detection.
	DetectRenames bool
	// RenameScore is the threshold to of similarity between files to consider
	// that a pair of delete and insert are a rename. The number must be
	// exactly between 0 and 100.
	RenameScore uint
	// RenameLimit bounds inexact (content-similarity) rename detection the
	// way git's diff.renameLimit does: after exact renames are paired off,
	// detection is skipped when the remaining added and deleted counts
	// are both above the limit, or when added*deleted > RenameLimit^2
	// (git's too_many_rename_candidates in diffcore-rename.c). Exact renames
	// are always detected. git's default is 1000; see DefaultRenameLimit.
	// A value of 0 means no limit.
	RenameLimit uint
	// OnlyExactRenames performs only detection of exact renames and will not perform
	// any detection of renames based on file similarity.
	OnlyExactRenames bool
}

// DefaultRenameLimit is git's default for diff.renameLimit. It is not applied
// by DefaultDiffTreeOptions, which keeps RenameLimit 0 (no limit) for
// compatibility.
const DefaultRenameLimit uint = 1000

// DefaultDiffTreeOptions are the default and recommended options for the
// diff tree.
var DefaultDiffTreeOptions = &DiffTreeOptions{
	DetectRenames:    true,
	RenameScore:      60,
	RenameLimit:      0,
	OnlyExactRenames: false,
}

// DiffTreeWithOptions compares the content and mode of the blobs found
// via two tree objects with the given options. The provided context
// must be non-nil.
// If no options are passed, no rename detection will be performed. The
// recommended options are DefaultDiffTreeOptions.
// An error will be returned if the context expires.
// This function will be deprecated and removed in v6 so the default
// behaviour of DiffTree is to detect renames.
func DiffTreeWithOptions(
	ctx context.Context,
	a, b *Tree,
	opts *DiffTreeOptions,
) (Changes, error) {
	from := NewTreeRootNode(a)
	to := NewTreeRootNode(b)

	hashEqual := func(a, b noder.Hasher) bool {
		return bytes.Equal(a.Hash(), b.Hash())
	}

	merkletrieChanges, err := merkletrie.DiffTreeContext(ctx, from, to, hashEqual)
	if err != nil {
		if err == merkletrie.ErrCanceled {
			return nil, ErrCanceled
		}
		return nil, err
	}

	changes, err := newChanges(merkletrieChanges)
	if err != nil {
		return nil, err
	}

	if opts == nil {
		opts = new(DiffTreeOptions)
	}

	if opts.DetectRenames {
		return DetectRenames(changes, opts)
	}

	return changes, nil
}
