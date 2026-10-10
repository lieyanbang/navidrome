package stream

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/navidrome/navidrome/utils/cache"
)

// PermanentAudioCache stores finished transcodes forever and exposes bytes as
// they are appended to a temporary file. A temporary file is never considered
// a cache hit and is only renamed after the producer and its Close both succeed.
type PermanentAudioCache struct {
	folder    string
	getReader cache.ReadFunc
	initErr   error
	writeFile func(*os.File, []byte) (int, error)

	mu   sync.Mutex
	jobs map[string]*permanentCacheEntry
}

func NewPermanentAudioCache(folder string, getReader cache.ReadFunc) *PermanentAudioCache {
	c := &PermanentAudioCache{
		folder: folder, getReader: getReader, jobs: map[string]*permanentCacheEntry{},
		writeFile: func(file *os.File, p []byte) (int, error) { return file.Write(p) },
	}
	if err := os.MkdirAll(folder, 0o700); err != nil {
		c.initErr = fmt.Errorf("create permanent transcoding cache: %w", err)
		return c
	}
	if err := c.removeOrphanParts(); err != nil {
		c.initErr = fmt.Errorf("clean incomplete transcoding cache files: %w", err)
	}
	return c
}

func (c *PermanentAudioCache) Available(context.Context) bool { return c.initErr == nil }
func (c *PermanentAudioCache) Disabled(context.Context) bool  { return c.initErr != nil }

func (c *PermanentAudioCache) Get(ctx context.Context, item cache.Item) (*cache.CachedStream, error) {
	if c.initErr != nil {
		return nil, c.initErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(item.Key())))
	finalPath := filepath.Join(c.folder, key+".mp3")
	for {
		if cached, err := openCompletedCache(finalPath); err == nil {
			return cached, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		c.mu.Lock()
		entry := c.jobs[key]
		if entry != nil {
			entry.mu.Lock()
			if entry.abandoned || entry.finished {
				changed := entry.changed
				finished := entry.finished
				producerErr := entry.err
				entry.mu.Unlock()
				c.mu.Unlock()
				if finished && producerErr != nil {
					return nil, producerErr
				}
				if finished {
					continue
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-changed:
					continue
				}
			}
			entry.readers++
			reader, err := os.Open(entry.partPath)
			entry.mu.Unlock()
			c.mu.Unlock()
			if err != nil {
				entry.detach()
				if errors.Is(err, os.ErrNotExist) {
					continue // The producer atomically published while the reader attached.
				}
				return nil, fmt.Errorf("open transcode cache reader: %w", err)
			}
			if err := entry.waitForStart(ctx); err != nil {
				_ = reader.Close()
				entry.detach()
				return nil, err
			}
			r := &permanentCacheReader{ctx: ctx, entry: entry, file: reader}
			return &cache.CachedStream{Reader: r, Closer: r, Cached: false}, nil
		}

		partPath := filepath.Join(c.folder, key+".part")
		file, err := os.OpenFile(partPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if errors.Is(err, os.ErrExist) {
			// A prior process may have died between startup cleanup and this Get.
			// Never adopt that partial output; remove it and create a fresh writer.
			if removeErr := os.Remove(partPath); removeErr != nil {
				c.mu.Unlock()
				return nil, fmt.Errorf("remove stale transcode part: %w", removeErr)
			}
			file, err = os.OpenFile(partPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		}
		if err != nil {
			c.mu.Unlock()
			return nil, fmt.Errorf("create transcode cache part: %w", err)
		}
		reader, err := os.Open(partPath)
		if err != nil {
			_ = file.Close()
			_ = os.Remove(partPath)
			c.mu.Unlock()
			return nil, fmt.Errorf("open transcode cache reader: %w", err)
		}
		producerCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		entry = &permanentCacheEntry{
			cache: c, key: key, finalPath: finalPath, partPath: partPath,
			file: file, ctx: producerCtx, cancel: cancel, changed: make(chan struct{}), started: make(chan struct{}), readers: 1,
		}
		c.jobs[key] = entry
		c.mu.Unlock()
		go entry.produce(item)
		if err := entry.waitForStart(ctx); err != nil {
			_ = reader.Close()
			entry.detach()
			return nil, err
		}
		r := &permanentCacheReader{ctx: ctx, entry: entry, file: reader}
		return &cache.CachedStream{Reader: r, Closer: r, Cached: false}, nil
	}
}

func openCompletedCache(path string) (*cache.CachedStream, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("completed transcode cache entry is not a regular file: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	sr := io.NewSectionReader(f, 0, info.Size())
	return &cache.CachedStream{Reader: sr, Seeker: sr, Closer: f, Cached: true}, nil
}

func (c *PermanentAudioCache) removeOrphanParts() error {
	entries, err := os.ReadDir(c.folder)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".part") {
			continue
		}
		if err := os.Remove(filepath.Join(c.folder, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

type permanentCacheEntry struct {
	cache     *PermanentAudioCache
	key       string
	finalPath string
	partPath  string
	file      *os.File
	ctx       context.Context
	cancel    context.CancelFunc

	mu        sync.Mutex
	changed   chan struct{}
	started   chan struct{}
	startErr  error
	readers   int
	finished  bool
	abandoned bool
	err       error
}

func (e *permanentCacheEntry) signalLocked() {
	close(e.changed)
	e.changed = make(chan struct{})
}

func (e *permanentCacheEntry) append(p []byte) (int, error) {
	n, err := e.cache.writeFile(e.file, p)
	if n > 0 {
		e.mu.Lock()
		e.signalLocked()
		e.mu.Unlock()
	}
	return n, err
}

func (e *permanentCacheEntry) waitForStart(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-e.started:
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.startErr
}

func (e *permanentCacheEntry) produce(item cache.Item) {
	reader, err := e.cache.getReader(e.ctx, item)
	if err == nil && reader == nil {
		err = errors.New("transcode producer returned a nil reader")
	}
	e.mu.Lock()
	e.startErr = err
	close(e.started)
	e.mu.Unlock()
	if err == nil {
		_, err = io.Copy(cacheEntryWriter{entry: e}, reader)
	}
	if closer, ok := reader.(io.Closer); ok {
		if closeErr := closer.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close transcoder output: %w", closeErr))
		}
	}
	if err == nil {
		var info os.FileInfo
		info, err = e.file.Stat()
		if err == nil && info.Size() == 0 {
			err = errors.New("transcoder produced an empty output")
		}
		if err == nil {
			err = e.file.Sync()
		}
	}
	if closeErr := e.file.Close(); closeErr != nil {
		err = errors.Join(err, fmt.Errorf("close transcode cache part: %w", closeErr))
	}
	if err == nil {
		err = os.Rename(e.partPath, e.finalPath)
	}
	if err == nil {
		err = syncCacheDirectory(e.cache.folder)
	}
	if err != nil {
		_ = os.Remove(e.partPath)
	}
	e.mu.Lock()
	e.finished = true
	e.err = err
	e.signalLocked()
	e.mu.Unlock()
	e.cancel()

	e.cache.mu.Lock()
	if e.cache.jobs[e.key] == e {
		delete(e.cache.jobs, e.key)
	}
	e.cache.mu.Unlock()
}

type cacheEntryWriter struct{ entry *permanentCacheEntry }

func (w cacheEntryWriter) Write(p []byte) (int, error) { return w.entry.append(p) }

func (e *permanentCacheEntry) detach() {
	e.mu.Lock()
	if e.readers > 0 {
		e.readers--
	}
	if e.readers == 0 && !e.finished {
		e.abandoned = true
		e.cancel()
	}
	e.mu.Unlock()
}

func syncCacheDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	err = dir.Sync()
	if closeErr := dir.Close(); err == nil {
		err = closeErr
	}
	if runtime.GOOS == "windows" || errors.Is(err, os.ErrInvalid) {
		// Windows and some mounted filesystems do not expose directory fsync.
		// The cache remains atomic there; Linux filesystems that support fsync
		// still get durable directory-entry publication.
		return nil
	}
	return err
}

type permanentCacheReader struct {
	ctx    context.Context
	entry  *permanentCacheEntry
	file   *os.File
	offset int64
	closed atomic.Bool
}

func (r *permanentCacheReader) Read(p []byte) (int, error) {
	if r.closed.Load() {
		return 0, os.ErrClosed
	}
	for {
		if err := r.ctx.Err(); err != nil {
			_ = r.Close()
			return 0, err
		}
		n, readErr := r.file.ReadAt(p, r.offset)
		if n > 0 {
			r.offset += int64(n)
			return n, nil
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return 0, readErr
		}
		e := r.entry
		e.mu.Lock()
		if e.finished {
			err := e.err
			e.mu.Unlock()
			if err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		changed := e.changed
		e.mu.Unlock()
		select {
		case <-r.ctx.Done():
			_ = r.Close()
			return 0, r.ctx.Err()
		case <-changed:
		}
	}
}

func (r *permanentCacheReader) Close() error {
	if !r.closed.CompareAndSwap(false, true) {
		return nil
	}
	err := r.file.Close()
	r.entry.detach()
	return err
}
