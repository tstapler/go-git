package object

import (
	"errors"
	"io"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/utils/ioutil"
	"github.com/go-git/go-git/v5/utils/merkletrie"
)

// DetectRenames detects the renames in the given changes on two trees with
// the given options. It will return the given changes grouping additions and
// deletions into modifications when possible.
// If options is nil, the default diff tree options will be used.
func DetectRenames(
	changes Changes,
	opts *DiffTreeOptions,
) (Changes, error) {
	if opts == nil {
		opts = DefaultDiffTreeOptions
	}

	detector := &renameDetector{
		renameScore: int(opts.RenameScore),
		renameLimit: int(opts.RenameLimit),
		onlyExact:   opts.OnlyExactRenames,
	}

	for _, c := range changes {
		action, err := c.Action()
		if err != nil {
			return nil, err
		}

		switch action {
		case merkletrie.Insert:
			detector.added = append(detector.added, c)
		case merkletrie.Delete:
			detector.deleted = append(detector.deleted, c)
		default:
			detector.modified = append(detector.modified, c)
		}
	}

	return detector.detect()
}

// renameDetector will detect and resolve renames in a set of changes.
// see: https://github.com/eclipse/jgit/blob/master/org.eclipse.jgit/src/org/eclipse/jgit/diff/RenameDetector.java
type renameDetector struct {
	added    []*Change
	deleted  []*Change
	modified []*Change

	renameScore int
	renameLimit int
	onlyExact   bool
}

// detectExactRenames pairs deleted and added files with identical content and a
// compatible file type, the way git's find_exact_renames/find_identical_files
// (diffcore-rename.c) does. Destinations are visited in path order; for each,
// the unused source with the same basename wins, otherwise the first unused
// source in path order. It is never limited by RenameLimit.
func (d *renameDetector) detectExactRenames() {
	sortChangesByName(d.added)
	sortChangesByName(d.deleted)

	srcsByHash := groupChangesByHash(d.deleted)
	used := make(map[*Change]struct{})
	var addedLeft []*Change

	for _, add := range d.added {
		var best *Change
		bestScore := 0
		for _, src := range srcsByHash[changeHash(add)] {
			if _, ok := used[src]; ok || !sameMode(src, add) {
				continue
			}
			// git: score = !rename_used (always 1 here) + basename_same.
			score := 1 + basenameSame(src, add)
			if score > bestScore {
				best, bestScore = src, score
				if score == 2 {
					break
				}
			}
		}
		if best == nil {
			addedLeft = append(addedLeft, add)
			continue
		}
		used[best] = struct{}{}
		d.modified = append(d.modified, &Change{From: best.From, To: add.To})
	}

	var deletedLeft []*Change
	for _, del := range d.deleted {
		if _, ok := used[del]; !ok {
			deletedLeft = append(deletedLeft, del)
		}
	}
	d.added, d.deleted = addedLeft, deletedLeft
}

func sortChangesByName(changes []*Change) {
	sort.SliceStable(changes, func(i, j int) bool { return changeName(changes[i]) < changeName(changes[j]) })
}

func baseName(path string) string {
	return path[strings.LastIndexByte(path, '/')+1:]
}

// basenameSame is git's basename_same: 1 when both paths end in the same file name.
func basenameSame(src, dst *Change) int {
	if baseName(changeName(src)) == baseName(changeName(dst)) {
		return 1
	}
	return 0
}

// detectBasenameRenames mirrors git's find_basename_matches (diffcore-rename.c): before
// the exhaustive matrix (and before the rename limit applies), a deleted and an added
// file whose basename is unique among the remaining deleted and among the remaining
// added files are paired when their content similarity reaches the minimum score.
func (d *renameDetector) detectBasenameRenames() error {
	srcBase := uniqueBasenames(d.deleted)
	dstBase := uniqueBasenames(d.added)

	usedSrc := make(map[int]struct{})
	usedDst := make(map[int]struct{})
	for dstIdx, add := range d.added {
		base := baseName(changeName(add))
		if dstBase[base] != dstIdx {
			continue
		}
		srcIdx, ok := srcBase[base]
		if !ok || srcIdx < 0 {
			continue
		}
		score, ok, err := pairSimilarity(d.deleted[srcIdx], add, d.renameScore)
		if err != nil {
			return err
		}
		if !ok || score < d.renameScore*100 {
			continue
		}
		usedSrc[srcIdx] = struct{}{}
		usedDst[dstIdx] = struct{}{}
		d.modified = append(d.modified, &Change{From: d.deleted[srcIdx].From, To: add.To})
	}

	var addedLeft, deletedLeft []*Change
	for i, c := range d.added {
		if _, ok := usedDst[i]; !ok {
			addedLeft = append(addedLeft, c)
		}
	}
	for i, c := range d.deleted {
		if _, ok := usedSrc[i]; !ok {
			deletedLeft = append(deletedLeft, c)
		}
	}
	d.added, d.deleted = addedLeft, deletedLeft
	return nil
}

// uniqueBasenames maps each basename to its index, or -1 when it occurs more than once.
func uniqueBasenames(changes []*Change) map[string]int {
	m := make(map[string]int, len(changes))
	for i, c := range changes {
		base := baseName(changeName(c))
		if _, dup := m[base]; dup {
			m[base] = -1
		} else {
			m[base] = i
		}
	}
	return m
}

// pairSimilarity scores one source/destination pair on the 0..10000 scale. ok is false when
// the pair cannot be a content rename (not two regular files, sizes too different, or a
// file too large to index).
func pairSimilarity(src, dst *Change, renameScore int) (score int, ok bool, err error) {
	if !isRegularFile(changeMode(src)) || !isRegularFile(changeMode(dst)) {
		return 0, false, nil
	}
	from, _, err := src.Files()
	if err != nil {
		return 0, false, err
	}
	_, to, err := dst.Files()
	if err != nil {
		return 0, false, err
	}
	srcSize, dstSize := from.Size+1, to.Size+1
	lo, hi := srcSize, dstSize
	if dstSize < srcSize {
		lo, hi = dstSize, srcSize
	}
	if int(lo*100/hi) < renameScore {
		return 0, false, nil
	}
	si, err := fileSimilarityIndex(from)
	if err == errIndexFull {
		return 0, false, nil
	} else if err != nil {
		return 0, false, err
	}
	di, err := fileSimilarityIndex(to)
	if err == errIndexFull {
		return 0, false, nil
	} else if err != nil {
		return 0, false, err
	}
	return si.score(di, 10000), true, nil
}

// detectContentRenames detects renames based on the similarity of the content
// in the files by building a matrix of pairs between sources and destinations
// and matching by the highest score.
// see: https://github.com/eclipse/jgit/blob/master/org.eclipse.jgit/src/org/eclipse/jgit/diff/SimilarityRenameDetector.java
func (d *renameDetector) detectContentRenames() error {
	if tooManyRenameCandidates(len(d.added), len(d.deleted), d.renameLimit) {
		return nil
	}

	srcs, dsts := d.deleted, d.added
	matrix, err := buildSimilarityMatrix(srcs, dsts, d.renameScore)
	if err != nil {
		return err
	}
	renames := make([]*Change, 0, min(len(matrix), len(dsts)))

	// Match rename pairs best first until everything above the minimum score has been
	// looked at. Like git, only each destination's best numCandidatesPerDst sources are
	// candidates (NUM_CANDIDATE_PER_DST in diffcore-rename.c).
	seen := make(map[int]int)
	for i := len(matrix) - 1; i >= 0; i-- {
		pair := matrix[i]
		seen[pair.added]++
		if seen[pair.added] > numCandidatesPerDst {
			continue
		}
		src := srcs[pair.deleted]
		dst := dsts[pair.added]

		if dst == nil || src == nil {
			// It was already matched before
			continue
		}

		renames = append(renames, &Change{From: src.From, To: dst.To})

		// Claim destination and source as matched
		dsts[pair.added] = nil
		srcs[pair.deleted] = nil
	}

	d.modified = append(d.modified, renames...)
	d.added = compactChanges(dsts)
	d.deleted = compactChanges(srcs)

	return nil
}

// tooManyRenameCandidates reports whether inexact (content-similarity) rename
// detection must be skipped for numDst remaining added files and numSrc
// remaining deleted files. It mirrors git's too_many_rename_candidates() in
// diffcore-rename.c for the non-copy case: detection runs only while
// (numDst <= limit || numSrc <= limit) && numDst*numSrc <= limit*limit, so
// 1 added x 5000 deleted still runs under the default limit of 1000, while
// 1100 x 1100 does not. The counts are those left after exact renames were
// paired off, so exact renames are always detected. A limit of 0 means no
// limit (git's own 0 or negative maps to 32767; pass that to match exactly).
//
// Not mirrored: git's basename-guess pre-pass (diffcore-rename.c
// find_basename_matches), which can pair files by basename before this check.
func tooManyRenameCandidates(numDst, numSrc, limit int) bool {
	if limit <= 0 {
		return false
	}
	dst, src, l := uint64(numDst), uint64(numSrc), uint64(limit)
	return (dst > l && src > l) || dst*src > l*l
}

func (d *renameDetector) detect() (Changes, error) {
	if len(d.added) > 0 && len(d.deleted) > 0 {
		d.detectExactRenames()

		if !d.onlyExact {
			if err := d.detectBasenameRenames(); err != nil {
				return nil, err
			}
			if err := d.detectContentRenames(); err != nil {
				return nil, err
			}
		}
	}

	result := make(Changes, 0, len(d.added)+len(d.deleted)+len(d.modified))
	result = append(result, d.added...)
	result = append(result, d.deleted...)
	result = append(result, d.modified...)

	sort.Stable(result)

	return result, nil
}

func bestNameMatch(change *Change, changes []*Change) *Change {
	var best *Change
	var bestScore int

	cname := changeName(change)

	for _, c := range changes {
		score := nameSimilarityScore(cname, changeName(c))
		if score > bestScore {
			bestScore = score
			best = c
		}
	}

	return best
}

func nameSimilarityScore(a, b string) int {
	aDirLen := strings.LastIndexByte(a, '/') + 1
	bDirLen := strings.LastIndexByte(b, '/') + 1

	dirMin := min(aDirLen, bDirLen)
	dirMax := max(aDirLen, bDirLen)

	var dirScoreLtr, dirScoreRtl int
	if dirMax == 0 {
		dirScoreLtr = 100
		dirScoreRtl = 100
	} else {
		var dirSim int

		for ; dirSim < dirMin; dirSim++ {
			if a[dirSim] != b[dirSim] {
				break
			}
		}

		dirScoreLtr = dirSim * 100 / dirMax

		if dirScoreLtr == 100 {
			dirScoreRtl = 100
		} else {
			for dirSim = 0; dirSim < dirMin; dirSim++ {
				if a[aDirLen-1-dirSim] != b[bDirLen-1-dirSim] {
					break
				}
			}
			dirScoreRtl = dirSim * 100 / dirMax
		}
	}

	fileMin := min(len(a)-aDirLen, len(b)-bDirLen)
	fileMax := max(len(a)-aDirLen, len(b)-bDirLen)

	fileSim := 0
	for ; fileSim < fileMin; fileSim++ {
		if a[len(a)-1-fileSim] != b[len(b)-1-fileSim] {
			break
		}
	}
	fileScore := fileSim * 100 / fileMax

	return (((dirScoreLtr + dirScoreRtl) * 25) + (fileScore * 50)) / 100
}

func changeName(c *Change) string {
	if c.To != empty {
		return c.To.Name
	}
	return c.From.Name
}

func changeHash(c *Change) plumbing.Hash {
	if c.To != empty {
		return c.To.TreeEntry.Hash
	}

	return c.From.TreeEntry.Hash
}

func changeMode(c *Change) filemode.FileMode {
	if c.To != empty {
		return c.To.TreeEntry.Mode
	}

	return c.From.TreeEntry.Mode
}

// sameMode reports whether two changes may be paired as an exact rename. Like git
// (diffcore-rename.c), a regular and an executable file pair with each other; a
// symlink pairs only with a symlink.
func sameMode(a, b *Change) bool {
	ma, mb := changeMode(a), changeMode(b)
	return ma == mb || (isRegularFile(ma) && isRegularFile(mb))
}

// isRegularFile is git's S_ISREG: regular and executable files, not symlinks or submodules.
func isRegularFile(m filemode.FileMode) bool {
	return m == filemode.Regular || m == filemode.Executable || m == filemode.Deprecated
}

func groupChangesByHash(changes []*Change) map[plumbing.Hash][]*Change {
	var result = make(map[plumbing.Hash][]*Change)
	for _, c := range changes {
		hash := changeHash(c)
		result[hash] = append(result[hash], c)
	}
	return result
}

// numCandidatesPerDst is git's NUM_CANDIDATE_PER_DST.
const numCandidatesPerDst = 4

type similarityMatrix []similarityPair

func (m similarityMatrix) Len() int      { return len(m) }
func (m similarityMatrix) Swap(i, j int) { m[i], m[j] = m[j], m[i] }

// Less orders by score, then by basename match (git's name_score), then prefers lower
// indices, so iterating from the end picks the first path in sorted order on a tie.
func (m similarityMatrix) Less(i, j int) bool {
	if m[i].score != m[j].score {
		return m[i].score < m[j].score
	}
	if m[i].name != m[j].name {
		return m[i].name < m[j].name
	}
	if m[i].added != m[j].added {
		return m[i].added > m[j].added
	}
	return m[i].deleted > m[j].deleted
}

type similarityPair struct {
	// index of the added file
	added int
	// index of the deleted file
	deleted int
	// similarity score
	score int
	// 1 when both files have the same basename (git's name_score)
	name int
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

const maxMatrixSize = 10000

// maxCachedIndexBytes bounds the memory held by cached destination similarity
// indexes (each is at most 1MiB). Past it, an index is rebuilt per use, which
// is the pre-cache behaviour, so results never depend on the budget.
const maxCachedIndexBytes = 128 << 20

// similarityIndexCache builds each destination's similarity index once and
// reuses it for every source, like git's diffcore-rename.c, which computes a
// filespec's spanhash once (diff_filespec_populate / estimate_similarity via
// cnt_data) and reuses it across all pairs.
type similarityIndexCache struct {
	indexes map[int]*similarityIndex
	bytes   int
	budget  int
}

func newSimilarityIndexCache(budget int) *similarityIndexCache {
	return &similarityIndexCache{indexes: make(map[int]*similarityIndex), budget: budget}
}

// get returns the index for dst number idx, building it from file() when absent.
func (c *similarityIndexCache) get(idx int, file func() (*File, error)) (*similarityIndex, error) {
	if di, ok := c.indexes[idx]; ok {
		return di, nil
	}
	f, err := file()
	if err != nil {
		return nil, err
	}
	di, err := fileSimilarityIndex(f)
	if err != nil {
		return nil, err
	}
	if size := len(di.hashes) * 8; c.bytes+size <= c.budget {
		c.indexes[idx] = di
		c.bytes += size
	}
	return di, nil
}

func buildSimilarityMatrix(srcs, dsts []*Change, renameScore int) (similarityMatrix, error) {
	return buildSimilarityMatrixWithBudget(srcs, dsts, renameScore, maxCachedIndexBytes)
}

// buildSimilarityMatrixWithBudget is buildSimilarityMatrix with an explicit
// destination-index cache budget in bytes; the result never depends on it.
func buildSimilarityMatrixWithBudget(srcs, dsts []*Change, renameScore, cacheBudget int) (similarityMatrix, error) {
	dstIndexes := newSimilarityIndexCache(cacheBudget)
	// Allocate for the worst-case scenario where every pair has a score
	// that we need to consider. We might not need that many.
	matrixSize := len(srcs) * len(dsts)
	if matrixSize > maxMatrixSize {
		matrixSize = maxMatrixSize
	}
	matrix := make(similarityMatrix, 0, matrixSize)
	srcSizes := make([]int64, len(srcs))
	dstSizes := make([]int64, len(dsts))
	dstTooLarge := make(map[int]bool)

	// Consider each pair of files, if the score is above the minimum
	// threshold we need to record that scoring in the matrix so we can
	// later find the best matches.
outerLoop:
	for srcIdx, src := range srcs {
		if !isRegularFile(changeMode(src)) {
			continue
		}

		// Declare the from file and the similarity index here to be able to
		// reuse it inside the inner loop. The reason to not initialize them
		// here is so we can skip the initialization in case they happen to
		// not be needed later. They will be initialized inside the inner
		// loop if and only if they're needed and reused in subsequent passes.
		var from *File
		var s *similarityIndex
		var err error
		for dstIdx, dst := range dsts {
			if !isRegularFile(changeMode(dst)) {
				continue
			}

			if dstTooLarge[dstIdx] {
				continue
			}

			var to *File
			srcSize := srcSizes[srcIdx]
			if srcSize == 0 {
				from, _, err = src.Files()
				if err != nil {
					return nil, err
				}
				srcSize = from.Size + 1
				srcSizes[srcIdx] = srcSize
			}

			dstSize := dstSizes[dstIdx]
			if dstSize == 0 {
				_, to, err = dst.Files()
				if err != nil {
					return nil, err
				}
				dstSize = to.Size + 1
				dstSizes[dstIdx] = dstSize
			}

			min, max := srcSize, dstSize
			if dstSize < srcSize {
				min = dstSize
				max = srcSize
			}

			if int(min*100/max) < renameScore {
				// File sizes are too different to be a match
				continue
			}

			if s == nil {
				s, err = fileSimilarityIndex(from)
				if err != nil {
					if err == errIndexFull {
						continue outerLoop
					}
					return nil, err
				}
			}

			di, err := dstIndexes.get(dstIdx, func() (*File, error) {
				if to != nil {
					return to, nil
				}
				_, f, err := dst.Files()
				return f, err
			})
			if err != nil {
				if err == errIndexFull {
					dstTooLarge[dstIdx] = true
				}

				return nil, err
			}

			// Content similarity only, 0..10000, as git does; basename equality is a
			// separate tie-break (name), not blended into the score.
			score := s.score(di, 10000)
			if score < renameScore*100 {
				continue
			}

			matrix = append(matrix, similarityPair{added: dstIdx, deleted: srcIdx, score: score, name: basenameSame(src, dst)})
		}
	}

	sort.Stable(matrix)

	return matrix, nil
}

func compactChanges(changes []*Change) []*Change {
	var result []*Change
	for _, c := range changes {
		if c != nil {
			result = append(result, c)
		}
	}
	return result
}

const (
	keyShift      = 32
	maxCountValue = (1 << keyShift) - 1
)

var errIndexFull = errors.New("index is full")

// similarityIndex is an index structure of lines/blocks in one file.
// This structure can be used to compute an approximation of the similarity
// between two files.
// To save space in memory, this index uses a space efficient encoding which
// will not exceed 1MiB per instance. The index starts out at a smaller size
// (closer to 2KiB), but may grow as more distinct blocks within the scanned
// file are discovered.
// see: https://github.com/eclipse/jgit/blob/master/org.eclipse.jgit/src/org/eclipse/jgit/diff/SimilarityIndex.java
type similarityIndex struct {
	hashed uint64
	// number of non-zero entries in hashes
	numHashes int
	growAt    int
	hashes    []keyCountPair
	hashBits  int
}

func fileSimilarityIndex(f *File) (*similarityIndex, error) {
	idx := newSimilarityIndex()
	if err := idx.hash(f); err != nil {
		return nil, err
	}

	sort.Stable(keyCountPairs(idx.hashes))

	return idx, nil
}

func newSimilarityIndex() *similarityIndex {
	return &similarityIndex{
		hashBits: 8,
		hashes:   make([]keyCountPair, 1<<8),
		growAt:   shouldGrowAt(8),
	}
}

func (i *similarityIndex) hash(f *File) error {
	isBin, err := f.IsBinary()
	if err != nil {
		return err
	}

	r, err := f.Reader()
	if err != nil {
		return err
	}

	defer ioutil.CheckClose(r, &err)

	return i.hashContent(r, f.Size, isBin)
}

func (i *similarityIndex) hashContent(r io.Reader, size int64, isBin bool) error {
	var buf = make([]byte, 4096)
	var ptr, cnt int
	remaining := size

	for 0 < remaining {
		hash := 5381
		var blockHashedCnt uint64

		// Hash one line or block, whatever happens first
		n := int64(0)
		for {
			if ptr == cnt {
				ptr = 0
				var err error
				cnt, err = io.ReadFull(r, buf)
				if err != nil && err != io.ErrUnexpectedEOF {
					return err
				}

				if cnt == 0 {
					return io.EOF
				}
			}
			n++
			c := buf[ptr] & 0xff
			ptr++

			// Ignore CR in CRLF sequence if it's text
			if !isBin && c == '\r' && ptr < cnt && buf[ptr] == '\n' {
				continue
			}
			blockHashedCnt++

			if c == '\n' {
				break
			}

			hash = (hash << 5) + hash + int(c)

			if n >= 64 || n >= remaining {
				break
			}
		}
		i.hashed += blockHashedCnt
		if err := i.add(hash, blockHashedCnt); err != nil {
			return err
		}
		remaining -= n
	}

	return nil
}

// score computes the similarity score between this index and another one.
// A region of a file is defined as a line in a text file or a fixed-size
// block in a binary file. To prepare an index, each region in the file is
// hashed; the values and counts of hashes are retained in a sorted table.
// Define the similarity fraction F as the count of matching regions between
// the two files divided between the maximum count of regions in either file.
// The similarity score is F multiplied by the maxScore constant, yielding a
// range [0, maxScore]. It is defined as maxScore for the degenerate case of
// two empty files.
// The similarity score is symmetrical; i.e. a.score(b) == b.score(a).
func (i *similarityIndex) score(other *similarityIndex, maxScore int) int {
	var maxHashed = i.hashed
	if maxHashed < other.hashed {
		maxHashed = other.hashed
	}
	if maxHashed == 0 {
		return maxScore
	}

	return int(i.common(other) * uint64(maxScore) / maxHashed)
}

func (i *similarityIndex) common(dst *similarityIndex) uint64 {
	srcIdx, dstIdx := 0, 0
	if i.numHashes == 0 || dst.numHashes == 0 {
		return 0
	}

	var common uint64
	srcKey, dstKey := i.hashes[srcIdx].key(), dst.hashes[dstIdx].key()

	for {
		if srcKey == dstKey {
			srcCnt, dstCnt := i.hashes[srcIdx].count(), dst.hashes[dstIdx].count()
			if srcCnt < dstCnt {
				common += srcCnt
			} else {
				common += dstCnt
			}

			srcIdx++
			if srcIdx == len(i.hashes) {
				break
			}
			srcKey = i.hashes[srcIdx].key()

			dstIdx++
			if dstIdx == len(dst.hashes) {
				break
			}
			dstKey = dst.hashes[dstIdx].key()
		} else if srcKey < dstKey {
			// Region of src that is not in dst
			srcIdx++
			if srcIdx == len(i.hashes) {
				break
			}
			srcKey = i.hashes[srcIdx].key()
		} else {
			// Region of dst that is not in src
			dstIdx++
			if dstIdx == len(dst.hashes) {
				break
			}
			dstKey = dst.hashes[dstIdx].key()
		}
	}

	return common
}

func (i *similarityIndex) add(key int, cnt uint64) error {
	key = int(uint32(key) * 0x9e370001 >> 1)

	j := i.slot(key)
	for {
		v := i.hashes[j]
		if v == 0 {
			// It's an empty slot, so we can store it here.
			if i.growAt <= i.numHashes {
				if err := i.grow(); err != nil {
					return err
				}
				j = i.slot(key)
				continue
			}

			var err error
			i.hashes[j], err = newKeyCountPair(key, cnt)
			if err != nil {
				return err
			}
			i.numHashes++
			return nil
		} else if v.key() == key {
			// It's the same key, so increment the counter.
			var err error
			i.hashes[j], err = newKeyCountPair(key, v.count()+cnt)
			return err
		} else if j+1 >= len(i.hashes) {
			j = 0
		} else {
			j++
		}
	}
}

type keyCountPair uint64

func newKeyCountPair(key int, cnt uint64) (keyCountPair, error) {
	if cnt > maxCountValue {
		return 0, errIndexFull
	}

	return keyCountPair((uint64(key) << keyShift) | cnt), nil
}

func (p keyCountPair) key() int {
	return int(p >> keyShift)
}

func (p keyCountPair) count() uint64 {
	return uint64(p) & maxCountValue
}

func (i *similarityIndex) slot(key int) int {
	// We use 31 - hashBits because the upper bit was already forced
	// to be 0 and we want the remaining high bits to be used as the
	// table slot.
	return int(uint32(key) >> uint(31-i.hashBits))
}

func shouldGrowAt(hashBits int) int {
	return (1 << uint(hashBits)) * (hashBits - 3) / hashBits
}

func (i *similarityIndex) grow() error {
	if i.hashBits == 30 {
		return errIndexFull
	}

	old := i.hashes

	i.hashBits++
	i.growAt = shouldGrowAt(i.hashBits)

	// TODO(erizocosmico): find a way to check if it will OOM and return
	// errIndexFull instead.
	i.hashes = make([]keyCountPair, 1<<uint(i.hashBits))

	for _, v := range old {
		if v != 0 {
			j := i.slot(v.key())
			for i.hashes[j] != 0 {
				j++
				if j >= len(i.hashes) {
					j = 0
				}
			}
			i.hashes[j] = v
		}
	}

	return nil
}

type keyCountPairs []keyCountPair

func (p keyCountPairs) Len() int           { return len(p) }
func (p keyCountPairs) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
func (p keyCountPairs) Less(i, j int) bool { return p[i] < p[j] }
