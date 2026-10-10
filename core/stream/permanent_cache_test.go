package stream

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils/cache"
)

type permanentTestItem string

func (i permanentTestItem) Key() string { return string(i) }

func permanentPath(folder string, item cache.Item) string {
	return filepath.Join(folder, fmt.Sprintf("%x.mp3", sha256.Sum256([]byte(item.Key()))))
}

func TestPermanentAudioCacheStreamsAndPersistsCompletedOutput(t *testing.T) {
	folder := t.TempDir()
	item := permanentTestItem("song-id|version-1|mp3-cbr-192-v1")
	reader, writer := io.Pipe()
	var starts atomic.Int32
	fc := NewPermanentAudioCache(folder, func(context.Context, cache.Item) (io.Reader, error) {
		starts.Add(1)
		return reader, nil
	})
	first, err := fc.Get(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	if first.Cached || first.Seeker != nil {
		t.Fatal("new output should stream from the active file without seek support")
	}

	writeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(writer, "first-bytes")
		writeDone <- err
	}()
	buf := make([]byte, 5)
	if _, err := io.ReadFull(first, buf); err != nil {
		t.Fatalf("reader did not receive bytes before encoder completion: %v", err)
	}
	if got := string(buf); got != "first" {
		t.Fatalf("got live prefix %q, want %q", got, "first")
	}
	if _, err := os.Stat(permanentPath(folder, item)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial output became a completed cache entry: stat err=%v", err)
	}

	second, err := fc.Get(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 {
		t.Fatalf("same key started %d producers, want one", starts.Load())
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	firstTail, err := io.ReadAll(first)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(append(buf, firstTail...)); got != "first-bytes" {
		t.Fatalf("first reader got %q", got)
	}
	secondData, err := io.ReadAll(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(secondData) != "first-bytes" {
		t.Fatalf("second reader got %q", secondData)
	}
	_ = first.Close()
	_ = second.Close()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(permanentPath(folder, item)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("completed output was not published")
		}
		time.Sleep(10 * time.Millisecond)
	}

	restarted := NewPermanentAudioCache(folder, func(context.Context, cache.Item) (io.Reader, error) {
		t.Fatal("restart should use the persistent completed entry")
		return nil, nil
	})
	hit, err := restarted.Get(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	defer hit.Close()
	if !hit.Cached || hit.Seeker == nil {
		t.Fatal("completed entry should be a seekable cache hit after restart")
	}
	data, err := io.ReadAll(hit)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "first-bytes" {
		t.Fatalf("persistent entry contains %q", data)
	}
}

type closeErrorReader struct{ closed bool }

func (r *closeErrorReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, io.EOF
	}
	r.closed = true
	return copy(p, []byte("partial")), nil
}

func (r *closeErrorReader) Close() error { return errors.New("encoder exit status was nonzero") }

func TestPermanentAudioCacheDoesNotPublishFailedEncoderOutput(t *testing.T) {
	folder := t.TempDir()
	item := permanentTestItem("failed-song")
	fc := NewPermanentAudioCache(folder, func(context.Context, cache.Item) (io.Reader, error) {
		return &closeErrorReader{}, nil
	})
	s, err := fc.Get(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(s)
	if err == nil || !strings.Contains(err.Error(), "nonzero") {
		t.Fatalf("reader error = %v, want encoder close failure", err)
	}
	if string(data) != "partial" {
		t.Fatalf("streamed bytes = %q", data)
	}
	_ = s.Close()
	if _, err := os.Stat(permanentPath(folder, item)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed transcode was published: stat err=%v", err)
	}
	entries, err := os.ReadDir(folder)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("incomplete transcode files remain: %v", entries)
	}
}

func TestPermanentAudioCacheHandlesDiskWriteFailure(t *testing.T) {
	folder := t.TempDir()
	item := permanentTestItem("disk-full-song")
	fc := NewPermanentAudioCache(folder, func(context.Context, cache.Item) (io.Reader, error) {
		return strings.NewReader("audio bytes"), nil
	})
	fc.writeFile = func(*os.File, []byte) (int, error) {
		return 0, errors.New("no space left on device")
	}
	s, err := fc.Get(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(s)
	if err == nil || !strings.Contains(err.Error(), "no space left on device") {
		t.Fatalf("reader error = %v, want disk write failure", err)
	}
	_ = s.Close()
	if _, err := os.Stat(permanentPath(folder, item)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disk-full output was published: stat err=%v", err)
	}
	entries, err := os.ReadDir(folder)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("disk-full partial output remains: %v", entries)
	}
}

func TestPermanentAudioCacheCancellationAndOrphanRecovery(t *testing.T) {
	folder := t.TempDir()
	item := permanentTestItem("cancel-song")
	orphan := filepath.Join(folder, "orphan.part")
	if err := os.WriteFile(orphan, []byte("truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	fc := NewPermanentAudioCache(folder, func(ctx context.Context, _ cache.Item) (io.Reader, error) {
		close(started)
		return cancellationReader{ctx: ctx}, nil
	})
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan part was not removed at startup: %v", err)
	}
	s, err := fc.Get(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(permanentPath(folder, item)); errors.Is(err, os.ErrNotExist) {
			entries, readErr := os.ReadDir(folder)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) == 0 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancelled transcode was not cleaned up: %v", entriesIn(t, folder))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type cancellationReader struct{ ctx context.Context }

func (r cancellationReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func (cancellationReader) Close() error { return nil }

func entriesIn(t *testing.T, folder string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(folder)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestStreamJobKeyIncludesSourceVersionAndPreset(t *testing.T) {
	base := streamJob{mf: &model.MediaFile{ID: "one"}, format: "mp3", bitRate: 192, sourceSize: 10, sourceMTime: 100}
	changed := base
	changed.sourceSize++
	if base.Key() == changed.Key() {
		t.Fatal("source size changes must invalidate the cache key")
	}
	changed = base
	changed.mf = &model.MediaFile{ID: "two"}
	if base.Key() == changed.Key() {
		t.Fatal("media identity changes must invalidate the cache key")
	}
}

func TestSourceValidatedReaderRejectsSourceMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	r := &sourceValidatedReadCloser{
		ReadCloser: io.NopCloser(strings.NewReader("encoded")),
		path:       path, size: info.Size(), mtime: info.ModTime().UnixNano(),
	}
	if err := os.WriteFile(path, []byte("changed-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err == nil || !strings.Contains(err.Error(), "changed during transcode") {
		t.Fatalf("close error = %v, want a source mutation error", err)
	}
}
