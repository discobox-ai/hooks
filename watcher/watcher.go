package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher observes a root directory recursively and emits debounced snapshot
// diffs on Batches.
type Watcher struct {
	root string
	opts Options

	fw *fsnotify.Watcher

	batches chan Batch
	errs    chan error

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	mu       sync.Mutex
	snapshot map[string]Entry
	watched  map[string]struct{}

	// reported is owned by New and then the run goroutine, not guarded by
	// mu. It holds the skipped paths whose error has been delivered on
	// Errors and that were still failing at the last full pass (scan plus
	// watch walk from the root). A path is reported when it first fails, not
	// again on every resync while it keeps failing, and again if it recovers
	// and fails later. An error dropped because Errors was full is not
	// recorded, so the next pass sends it again.
	reported map[string]struct{}
}

// skippedPaths collects the non-fatal per-path failures of one walk: a
// directory that could not be read or watched, or an entry that could not be
// stat'ed. Each error names its path.
type skippedPaths map[string]error

func (s skippedPaths) add(path string, err error) {
	if _, ok := s[path]; !ok {
		s[path] = err
	}
}

// New creates a recursive watcher rooted at root. root must name an existing
// directory. .git directories are always skipped.
func New(root string, opts Options) (*Watcher, error) {
	opts = normalizeOptions(opts)

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("watch root %q is not a directory", root)
	}

	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	w := &Watcher{
		root:     absRoot,
		opts:     opts,
		fw:       fw,
		batches:  make(chan Batch, opts.BatchBuffer),
		errs:     make(chan error, opts.ErrorBuffer),
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
		watched:  make(map[string]struct{}),
		reported: make(map[string]struct{}),
	}

	skipped := skippedPaths{}
	if opts.InitialSnapshot != nil {
		w.snapshot = cloneSnapshot(opts.InitialSnapshot)
	} else {
		w.snapshot, err = w.scan(nil, skipped)
		if err != nil {
			_ = fw.Close()
			cancel()
			return nil, err
		}
	}

	if err := w.addRecursive(absRoot, skipped); err != nil {
		_ = fw.Close()
		cancel()
		return nil, err
	}
	// Errors is buffered, so these wait there until the caller reads it.
	w.reportSkipped(skipped, true)

	go w.run(opts.EmitInitial && opts.InitialSnapshot == nil)
	return w, nil
}

// Batches returns the channel of debounced semantic filesystem changes. It is
// closed after Close completes.
func (w *Watcher) Batches() <-chan Batch { return w.batches }

// Errors returns asynchronous watcher errors. It is closed after Close
// completes.
func (w *Watcher) Errors() <-chan error { return w.errs }

// Close releases native watcher resources and closes Batches and Errors.
func (w *Watcher) Close() error {
	var err error
	w.once.Do(func() {
		w.cancel()
		err = w.fw.Close()
		<-w.done
	})
	return err
}

// Snapshot returns the watcher's current full repository snapshot.
func (w *Watcher) Snapshot() map[string]Entry {
	w.mu.Lock()
	defer w.mu.Unlock()
	return cloneSnapshot(w.snapshot)
}

func (w *Watcher) run(emitInitial bool) {
	defer close(w.done)
	defer close(w.batches)
	defer close(w.errs)

	if emitInitial {
		w.mu.Lock()
		snap := cloneSnapshot(w.snapshot)
		w.mu.Unlock()
		var empty map[string]Entry
		w.sendBatch(Batch{Changes: diffSnapshots(empty, snap), Resync: true, Snapshot: snap})
	}

	var debounce *time.Timer
	var debounceC <-chan time.Time
	var resync *time.Ticker
	if w.opts.PeriodicResync > 0 {
		resync = time.NewTicker(w.opts.PeriodicResync)
		defer resync.Stop()
	}

	schedule := func() {
		if debounce == nil {
			debounce = time.NewTimer(w.opts.Debounce)
			debounceC = debounce.C
			return
		}
		if !debounce.Stop() {
			select {
			case <-debounce.C:
			default:
			}
		}
		debounce.Reset(w.opts.Debounce)
		debounceC = debounce.C
	}

	for {
		select {
		case <-w.ctx.Done():
			if debounce != nil {
				debounce.Stop()
			}
			return
		case event, ok := <-w.fw.Events:
			if !ok {
				return
			}
			if w.shouldIgnoreAbs(event.Name) {
				continue
			}
			if event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
				w.addIfDirectory(event.Name)
			}
			schedule()
		case err, ok := <-w.fw.Errors:
			if !ok {
				return
			}
			w.sendError(err)
			schedule()
		case <-debounceC:
			debounceC = nil
			w.rescan(true)
		case <-tickerC(resync):
			w.rescan(true)
		}
	}
}

func tickerC(t *time.Ticker) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

func (w *Watcher) rescan(resync bool) {
	w.mu.Lock()
	prev := w.snapshot
	w.mu.Unlock()
	skipped := skippedPaths{}
	newSnap, err := w.scan(prev, skipped)
	if err != nil {
		w.sendError(err)
		return
	}
	if err := w.addRecursive(w.root, skipped); err != nil {
		w.sendError(err)
	}
	w.reportSkipped(skipped, true)

	w.mu.Lock()
	oldSnap := w.snapshot
	changes := diffSnapshots(oldSnap, newSnap)
	w.snapshot = newSnap
	w.mu.Unlock()

	if len(changes) > 0 {
		w.sendBatch(Batch{Changes: changes, Resync: resync, Snapshot: cloneSnapshot(newSnap)})
	}
}

func (w *Watcher) sendBatch(batch Batch) {
	select {
	case <-w.ctx.Done():
	case w.batches <- batch:
	}
}

// sendError delivers err on Errors without blocking, and reports whether it
// was delivered; it is dropped when the buffer is full.
func (w *Watcher) sendError(err error) bool {
	if err == nil {
		return false
	}
	select {
	case <-w.ctx.Done():
		return false
	case w.errs <- err:
		return true
	default:
		return false
	}
}

// reportSkipped sends an error for each skipped path not already reported,
// and records the ones delivered. After a full pass (full is true) a reported
// path that no longer fails is forgotten, so it is reported again if it fails
// later; a partial walk (a newly created directory) only adds to the set.
func (w *Watcher) reportSkipped(skipped skippedPaths, full bool) {
	paths := make([]string, 0, len(skipped))
	for path := range skipped {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	next := make(map[string]struct{}, len(skipped))
	if !full {
		for path := range w.reported {
			next[path] = struct{}{}
		}
	}
	for _, path := range paths {
		if _, ok := w.reported[path]; ok || w.sendError(skipped[path]) {
			next[path] = struct{}{}
		}
	}
	w.reported = next
}

// scan walks the tree into a fresh snapshot. Only a failure at the root fails
// the scan. A path below it that cannot be read is recorded in skipped and
// pruned, and its previous entries are carried forward from prevSnap: what
// the watcher cannot see is reported as unchanged rather than deleted.
func (w *Watcher) scan(prevSnap map[string]Entry, skipped skippedPaths) (map[string]Entry, error) {
	snap := make(map[string]Entry)
	err := filepath.WalkDir(w.root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			if path == w.root {
				return walkErr
			}
			// WalkDir calls back a second time for a directory it could not
			// read; the directory's own entry is already in snap.
			skipped.add(path, walkErr)
			carryForward(snap, prevSnap, w.relSlash(path))
			return fs.SkipDir
		}
		if path == w.root {
			return nil
		}
		rel := w.relSlash(path)

		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			skipped.add(path, &fs.PathError{Op: "stat", Path: path, Err: err})
			if prev, ok := prevSnap[rel]; ok {
				snap[rel] = prev
			}
			if d.IsDir() {
				carryForward(snap, prevSnap, rel)
				return fs.SkipDir
			}
			return nil
		}

		entry := Entry{Path: rel, IsDir: info.IsDir(), Size: info.Size(), Mode: info.Mode(), ModTime: info.ModTime()}
		if shouldSkipGit(rel, entry) || shouldSkipNodeModules(rel) || (w.opts.Ignore != nil && w.opts.Ignore(rel, entry)) {
			if entry.IsDir {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Mode.IsRegular() {
			// Reuse the previous content hash when size and mtime are unchanged so
			// we only read files whose stat actually moved. On a stat change (or an
			// unseeded/first scan) recompute; a read error leaves the hash empty and
			// falls back to metadata comparison.
			if prev, ok := prevSnap[rel]; ok && prev.Hash != "" && prev.Size == entry.Size && prev.ModTime.Equal(entry.ModTime) {
				entry.Hash = prev.Hash
			} else if h, herr := hashFile(path); herr == nil {
				entry.Hash = h
			}
		}
		snap[rel] = entry
		return nil
	})
	return snap, err
}

// BackfillHashes fills in the content hash of each regular-file entry in snap
// that has none, such as a checkpoint row saved before hashes were persisted,
// and reports how many it filled. It hashes only files whose size and mtime on
// disk still match the entry: those are the files the size-and-mtime fallback
// already treats as unchanged, so their current bytes are the entry's. A file
// that moved keeps its empty hash and is still reported as modified.
func BackfillHashes(root string, snap map[string]Entry) int {
	filled := 0
	for path, entry := range snap {
		if entry.Hash != "" || !entry.Mode.IsRegular() {
			continue
		}
		abs := filepath.Join(root, filepath.FromSlash(path))
		info, err := os.Lstat(abs)
		if err != nil || !info.Mode().IsRegular() || info.Size() != entry.Size || !info.ModTime().Equal(entry.ModTime) {
			continue
		}
		h, err := hashFile(abs)
		if err != nil {
			continue
		}
		entry.Hash = h
		snap[path] = entry
		filled++
	}
	return filled
}

// relSlash returns path relative to the root, slash-separated. path is always
// the root or below it, as WalkDir from the root produces.
func (w *Watcher) relSlash(path string) string {
	rel, err := filepath.Rel(w.root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

// carryForward copies prev's entries below dir into snap.
func carryForward(snap, prev map[string]Entry, dir string) {
	prefix := dir + "/"
	for rel, entry := range prev {
		if strings.HasPrefix(rel, prefix) {
			snap[rel] = entry
		}
	}
}

// hashFile returns a hex content digest for the regular file at path.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// addRecursive watches root and every directory below it. Only a failure at
// the watcher's own root is returned; a directory below it that cannot be
// read or watched is recorded in skipped and pruned, and the rest of the tree
// is still watched. The periodic resync still sees changes under a directory
// that is readable but not watched.
func (w *Watcher) addRecursive(root string, skipped skippedPaths) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if path == w.root {
				return err
			}
			skipped.add(path, err)
			return fs.SkipDir
		}
		if !d.IsDir() {
			return nil
		}
		if w.shouldIgnoreAbs(path) {
			if path == w.root {
				return nil
			}
			return fs.SkipDir
		}
		if err := w.addWatch(path); err != nil {
			if path == w.root {
				return err
			}
			skipped.add(path, err)
			return fs.SkipDir
		}
		return nil
	})
}

func (w *Watcher) addIfDirectory(path string) {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return
	}
	skipped := skippedPaths{}
	if err := w.addRecursive(path, skipped); err != nil {
		w.sendError(err)
	}
	w.reportSkipped(skipped, false)
}

func (w *Watcher) addWatch(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	w.mu.Lock()
	if _, ok := w.watched[abs]; ok {
		w.mu.Unlock()
		return nil
	}
	w.mu.Unlock()

	if err := w.fw.Add(abs); err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		// fsnotify's error does not name the path.
		return &fs.PathError{Op: "watch", Path: abs, Err: err}
	}

	w.mu.Lock()
	w.watched[abs] = struct{}{}
	w.mu.Unlock()
	return nil
}

func (w *Watcher) shouldIgnoreAbs(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	if abs == w.root {
		return false
	}
	rel, err := filepath.Rel(w.root, abs)
	if err != nil || rel == "." || rel == "" {
		return false
	}
	rel = filepath.ToSlash(rel)
	entry := Entry{Path: rel, IsDir: true}
	return shouldSkipGit(rel, entry) || shouldSkipNodeModules(rel) || (w.opts.Ignore != nil && w.opts.Ignore(rel, entry))
}

func shouldSkipGit(rel string, entry Entry) bool {
	if rel == "." || rel == "" {
		return false
	}
	if rel == ".git" || strings.HasPrefix(rel, ".git/") {
		return true
	}
	return strings.Contains(rel, "/.git/") || (entry.IsDir && strings.HasSuffix(rel, "/.git"))
}

func shouldSkipNodeModules(rel string) bool {
	if rel == "." || rel == "" {
		return false
	}
	for _, part := range strings.Split(rel, "/") {
		if part == "node_modules" {
			return true
		}
	}
	return false
}
