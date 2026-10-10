package ffmpeg

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const remoteWorkerPreset = "mp3-cbr-192-v1"

var errRemoteWorkerIncomplete = errors.New("remote FFmpeg worker stream ended without a verified completion marker")

type remoteWorkerFFmpeg struct {
	local    FFmpeg
	endpoint string
	key      []byte
	initErr  error
	client   *http.Client
}

type workerTokenClaims struct {
	Version      int    `json:"v"`
	MediaID      string `json:"media_id"`
	SourceSize   int64  `json:"source_size"`
	SourceMTime  int64  `json:"source_mtime_ns"`
	DurationMS   int64  `json:"duration_ms"`
	OffsetSecond int    `json:"offset_seconds"`
	Preset       string `json:"preset"`
	ExpiresAt    int64  `json:"exp"`
	Nonce        string `json:"nonce"`
}

func newRemoteWorkerFFmpeg(local FFmpeg, endpoint, secretFile string) FFmpeg {
	r := &remoteWorkerFFmpeg{
		local: local,
		client: &http.Client{Transport: &http.Transport{
			Proxy:                 nil,
			MaxConnsPerHost:       1,
			MaxIdleConnsPerHost:   1,
			ResponseHeaderTimeout: 15 * time.Second,
			IdleConnTimeout:       30 * time.Second,
		}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		r.initErr = errors.New("remote FFmpeg worker URL must be an absolute HTTP origin without credentials or a path")
		return r
	}
	r.endpoint = strings.TrimRight(endpoint, "/") + "/v1/transcode"
	if secretFile == "" {
		r.initErr = errors.New("remote FFmpeg worker secret file is not configured")
		return r
	}
	encodedKey, err := os.ReadFile(secretFile)
	if err != nil {
		r.initErr = fmt.Errorf("read remote FFmpeg worker secret file: %w", err)
		return r
	}
	r.key, err = hex.DecodeString(strings.TrimSpace(string(encodedKey)))
	if err != nil || len(r.key) < 32 {
		r.initErr = errors.New("remote FFmpeg worker secret must be at least 32 bytes encoded as hexadecimal")
	}
	return r
}

func (r *remoteWorkerFFmpeg) Transcode(ctx context.Context, opts TranscodeOptions) (io.ReadCloser, error) {
	if opts.Format != "mp3" {
		return r.local.Transcode(ctx, opts)
	}
	if r.initErr != nil {
		return nil, r.initErr
	}
	if opts.MediaID == "" || opts.SourceSize <= 0 || opts.SourceMTime == 0 || opts.Duration <= 0 || math.IsNaN(float64(opts.Duration)) || math.IsInf(float64(opts.Duration), 0) || opts.Offset < 0 {
		return nil, errors.New("remote FFmpeg worker requires a valid media identity, source fingerprint, duration, and offset")
	}
	file, err := os.Open(opts.FilePath)
	if err != nil {
		return nil, fmt.Errorf("open source for remote transcode: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat source for remote transcode: %w", err)
	}
	if info.Size() != opts.SourceSize || info.ModTime().UnixNano() != opts.SourceMTime {
		_ = file.Close()
		return nil, errors.New("source changed before remote transcode")
	}

	claims, err := makeWorkerTokenClaims(opts)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	token, err := r.signClaims(claims)
	if err != nil {
		_ = file.Close()
		return nil, err
	}

	requestCtx, cancel := context.WithCancel(ctx)
	pipeReader, pipeWriter := io.Pipe()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, r.endpoint, pipeReader)
	if err != nil {
		cancel()
		_ = file.Close()
		_ = pipeReader.Close()
		_ = pipeWriter.Close()
		return nil, err
	}
	request.ContentLength = opts.SourceSize
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("Accept", "audio/mpeg")

	uploadDone := make(chan error, 1)
	go func() {
		defer close(uploadDone)
		defer file.Close()
		written, copyErr := io.CopyN(pipeWriter, file, opts.SourceSize)
		if copyErr == nil && written != opts.SourceSize {
			copyErr = io.ErrUnexpectedEOF
		}
		if copyErr == nil {
			var extra [1]byte
			if n, readErr := file.Read(extra[:]); n != 0 || readErr == nil {
				copyErr = errors.New("source grew during remote upload")
			} else if !errors.Is(readErr, io.EOF) {
				copyErr = readErr
			}
		}
		if copyErr == nil {
			if latest, statErr := os.Stat(opts.FilePath); statErr != nil {
				copyErr = statErr
			} else if latest.Size() != opts.SourceSize || latest.ModTime().UnixNano() != opts.SourceMTime {
				copyErr = errors.New("source changed during remote upload")
			}
		}
		if copyErr != nil {
			_ = pipeWriter.CloseWithError(copyErr)
			uploadDone <- copyErr
			return
		}
		_ = pipeWriter.Close()
		uploadDone <- nil
	}()

	response, err := r.client.Do(request)
	if err != nil {
		cancel()
		_ = pipeReader.CloseWithError(err)
		_ = pipeWriter.CloseWithError(err)
		select {
		case <-uploadDone:
		case <-time.After(2 * time.Second):
		}
		return nil, fmt.Errorf("request remote FFmpeg worker: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		_ = response.Body.Close()
		cancel()
		_ = pipeReader.Close()
		_ = pipeWriter.Close()
		select {
		case <-uploadDone:
		case <-time.After(2 * time.Second):
		}
		return nil, fmt.Errorf("remote FFmpeg worker returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	if !strings.EqualFold(response.Header.Get("Content-Type"), "audio/mpeg") || len(response.TransferEncoding) == 0 || !strings.EqualFold(response.TransferEncoding[0], "chunked") {
		_ = response.Body.Close()
		cancel()
		_ = pipeReader.Close()
		_ = pipeWriter.Close()
		select {
		case <-uploadDone:
		case <-time.After(2 * time.Second):
		}
		return nil, errors.New("remote FFmpeg worker response is not a chunked MPEG stream")
	}
	return &remoteTranscodeReadCloser{
		body: response.Body, response: response, cancel: cancel,
		pipeReader: pipeReader, pipeWriter: pipeWriter, uploadDone: uploadDone,
		hash: sha256.New(),
	}, nil
}

func makeWorkerTokenClaims(opts TranscodeOptions) (workerTokenClaims, error) {
	var nonceBytes [18]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		return workerTokenClaims{}, fmt.Errorf("create remote transcode nonce: %w", err)
	}
	duration := int64(math.Ceil(float64(opts.Duration) * 1000))
	return workerTokenClaims{
		Version: 1, MediaID: opts.MediaID, SourceSize: opts.SourceSize,
		SourceMTime: opts.SourceMTime, DurationMS: duration,
		OffsetSecond: opts.Offset, Preset: remoteWorkerPreset,
		ExpiresAt: time.Now().Add(60 * time.Second).Unix(),
		Nonce:     base64.RawURLEncoding.EncodeToString(nonceBytes[:]),
	}, nil
}

func (r *remoteWorkerFFmpeg) signClaims(claims workerTokenClaims) (string, error) {
	data, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("encode remote transcode token: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(data)
	mac := hmac.New(sha256.New, r.key)
	_, _ = io.WriteString(mac, encoded)
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return encoded + "." + signature, nil
}

type remoteTranscodeReadCloser struct {
	body       io.ReadCloser
	response   *http.Response
	cancel     context.CancelFunc
	pipeReader *io.PipeReader
	pipeWriter *io.PipeWriter
	uploadDone <-chan error
	hash       hash.Hash
	bytesRead  int64

	mu           sync.Mutex
	finished     bool
	complete     bool
	terminalErr  error
	terminalSent bool
	closeOnce    sync.Once
	closeErr     error
}

func (r *remoteTranscodeReadCloser) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if n > 0 {
		_, _ = r.hash.Write(p[:n])
		r.bytesRead += int64(n)
	}
	if !errors.Is(err, io.EOF) {
		return n, err
	}

	r.mu.Lock()
	if !r.finished {
		r.finished = true
		r.terminalErr = r.validateCompletion()
		r.complete = r.terminalErr == nil
	}
	terminalErr := r.terminalErr
	if terminalErr != nil && !r.terminalSent {
		r.terminalSent = true
		r.mu.Unlock()
		return n, terminalErr
	}
	r.mu.Unlock()
	return n, io.EOF
}

func (r *remoteTranscodeReadCloser) validateCompletion() error {
	if len(r.response.TransferEncoding) == 0 || !strings.EqualFold(r.response.TransferEncoding[0], "chunked") || !strings.EqualFold(r.response.Trailer.Get("X-Transcode-Status"), "complete") {
		return errRemoteWorkerIncomplete
	}
	select {
	case uploadErr := <-r.uploadDone:
		if uploadErr != nil {
			return fmt.Errorf("upload source to remote worker: %w", uploadErr)
		}
	case <-time.After(2 * time.Second):
		return errors.New("remote worker completed before source upload closed")
	}
	if got, want := strings.ToLower(r.response.Trailer.Get("X-Output-SHA256")), hex.EncodeToString(r.hash.Sum(nil)); got != want {
		return errors.New("remote worker output checksum did not match")
	}
	outputBytes, err := strconv.ParseInt(r.response.Trailer.Get("X-Output-Bytes"), 10, 64)
	if err != nil || outputBytes != r.bytesRead || outputBytes <= 0 {
		return errors.New("remote worker output length did not match the completion trailer")
	}
	return nil
}

func (r *remoteTranscodeReadCloser) Close() error {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		complete := r.complete
		terminalErr := r.terminalErr
		r.mu.Unlock()
		if !complete {
			r.cancel()
			_ = r.pipeReader.Close()
			_ = r.pipeWriter.Close()
			if terminalErr == nil {
				terminalErr = errRemoteWorkerIncomplete
			}
			r.closeErr = errors.Join(terminalErr, r.body.Close())
			return
		}
		r.cancel()
		r.closeErr = r.body.Close()
	})
	return r.closeErr
}

func (r *remoteWorkerFFmpeg) ExtractImage(ctx context.Context, path string) (io.ReadCloser, error) {
	return r.local.ExtractImage(ctx, path)
}

func (r *remoteWorkerFFmpeg) ConvertAnimatedImage(ctx context.Context, reader io.Reader, maxSize int, quality int) (io.ReadCloser, error) {
	return r.local.ConvertAnimatedImage(ctx, reader, maxSize, quality)
}

func (r *remoteWorkerFFmpeg) ProbeAudioStream(ctx context.Context, filePath string) (*AudioProbeResult, error) {
	return r.local.ProbeAudioStream(ctx, filePath)
}

func (r *remoteWorkerFFmpeg) CmdPath() (string, error) { return r.local.CmdPath() }
func (r *remoteWorkerFFmpeg) IsAvailable() bool        { return r.local.IsAvailable() }
func (r *remoteWorkerFFmpeg) IsProbeAvailable() bool   { return r.local.IsProbeAvailable() }
func (r *remoteWorkerFFmpeg) Version() string          { return r.local.Version() }
