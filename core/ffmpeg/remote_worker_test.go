package ffmpeg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRemoteWorkerStreamsBeforeSourceUploadCompletes(t *testing.T) {
	const sourceSize = 32 * 1024 * 1024
	dir := t.TempDir()
	source := filepath.Join(dir, "source.flac")
	if err := os.WriteFile(source, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(source, sourceSize); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	secretFile := filepath.Join(dir, "worker.key")
	if err := os.WriteFile(secretFile, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	continueUpload := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Errorf("enable full duplex: %v", err)
			return
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Trailer", "X-Transcode-Status, X-Output-SHA256, X-Output-Bytes")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		if _, err := w.Write([]byte("mp3:")); err != nil {
			t.Errorf("write first output: %v", err)
			return
		}
		flusher.Flush()
		<-continueUpload
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("read upload: %v", err)
			return
		}
		sum := sha256.Sum256([]byte("mp3:"))
		w.Header().Set("X-Transcode-Status", "complete")
		w.Header().Set("X-Output-SHA256", hex.EncodeToString(sum[:]))
		w.Header().Set("X-Output-Bytes", "4")
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	remote := newRemoteWorkerFFmpeg(&ffmpeg{}, server.URL, secretFile)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := remote.Transcode(ctx, TranscodeOptions{
		Format: "mp3", FilePath: source, MediaID: "test-media-1",
		SourceSize: sourceSize, SourceMTime: info.ModTime().UnixNano(), Duration: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 4)
	if _, err := io.ReadFull(stream, first); err != nil {
		_ = stream.Close()
		t.Fatalf("read first output chunk before upload completed: %v", err)
	}
	if string(first) != "mp3:" {
		t.Fatalf("unexpected first output %q", first)
	}
	select {
	case <-stream.(*remoteTranscodeReadCloser).uploadDone:
		t.Fatal("worker buffered the complete source before returning output")
	default:
	}
	close(continueUpload)
	if _, err := io.Copy(io.Discard, stream); err != nil {
		t.Fatalf("read stream to verified completion: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close completed stream: %v", err)
	}
}

func TestRemoteWorkerRejectsFailedCompletionTrailer(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.flac")
	if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(source)
	secretFile := filepath.Join(dir, "worker.key")
	if err := os.WriteFile(secretFile, []byte(hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Trailer", "X-Transcode-Status, X-Output-SHA256, X-Output-Bytes")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		w.Header().Set("X-Transcode-Status", "failed")
		w.Header().Set("X-Output-SHA256", "")
		w.Header().Set("X-Output-Bytes", "0")
	}))
	defer server.Close()
	remote := newRemoteWorkerFFmpeg(&ffmpeg{}, server.URL, secretFile)
	stream, err := remote.Transcode(context.Background(), TranscodeOptions{
		Format: "mp3", FilePath: source, MediaID: "test-media-2",
		SourceSize: info.Size(), SourceMTime: info.ModTime().UnixNano(), Duration: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, stream)
	if readErr == nil {
		t.Fatal("failed trailer was treated as a successful transcode")
	}
	if closeErr := stream.Close(); closeErr == nil {
		t.Fatal("closing failed stream did not preserve the failure")
	}
}
