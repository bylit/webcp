package download

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (function roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type controlledBody struct {
	mu      sync.Mutex
	reads   int
	release <-chan struct{}
}

func (body *controlledBody) Read(buffer []byte) (int, error) {
	body.mu.Lock()
	body.reads++
	read := body.reads
	body.mu.Unlock()
	if read == 1 {
		return copy(buffer, bytes.Repeat([]byte("a"), 32*1024)), nil
	}
	if read == 2 {
		<-body.release
		return copy(buffer, bytes.Repeat([]byte("b"), 32*1024)), nil
	}
	return 0, io.EOF
}

// Close deliberately does not release a blocked read. Some transports can
// still return buffered data after cancellation, which is the race under test.
func (body *controlledBody) Close() error { return nil }

func TestDownloadPauseAndResume(t *testing.T) {
	payload := bytes.Repeat([]byte("webcp-range-test-"), 64*1024)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="remote-file.bin"`)
		start := int64(0)
		if value := r.Header.Get("Range"); value != "" {
			_, _ = fmt.Sscanf(value, "bytes=%d-", &start)
			if start >= int64(len(payload)) {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(payload)))
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(payload)-1, len(payload)))
			w.WriteHeader(http.StatusPartialContent)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)-int(start)))
		for position := start; position < int64(len(payload)); {
			end := min(position+16*1024, int64(len(payload)))
			_, _ = w.Write(payload[position:end])
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			position = end
			time.Sleep(time.Millisecond)
		}
	}))
	defer remote.Close()

	directory := t.TempDir()
	manager, err := NewManager(directory, filepath.Join(directory, "state", "jobs.json"), 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	job, err := manager.Create(remote.URL+"/original-name", directory)
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, 4*time.Second, func() bool {
		current, _ := manager.Get(job.ID)
		return current.Status == StatusDownloading && current.DownloadedBytes > 64*1024
	})
	paused, err := manager.Pause(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Status != StatusPaused {
		t.Fatalf("got status %q", paused.Status)
	}
	if _, err := manager.Resume(job.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		current, _ := manager.Get(job.ID)
		return current.Status == StatusCompleted
	})

	current, _ := manager.Get(job.ID)
	if current.Filename != "remote-file.bin" {
		t.Fatalf("got filename %q", current.Filename)
	}
	actual, err := os.ReadFile(filepath.Join(directory, current.Filename))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, payload) {
		t.Fatalf("download differs: got %d bytes, want %d", len(actual), len(payload))
	}
}

func TestServerWithoutRangeRestartsPartialFile(t *testing.T) {
	payload := []byte("complete remote content")
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer remote.Close()

	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "asset.dat"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(directory, filepath.Join(directory, ".state", "jobs.json"), 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	job := &Job{ID: "existing", URL: remote.URL, Filename: "asset.dat", Destination: directory, Status: StatusDownloading}
	manager.jobs[job.ID] = job
	if err := manager.transfer(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(directory, "asset.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, payload) {
		t.Fatalf("server ignored range and file was not restarted: %q", actual)
	}
}

func TestCreateValidationAndUniqueNames(t *testing.T) {
	directory := t.TempDir()
	manager, err := NewManager(directory, filepath.Join(directory, ".state", "jobs.json"), 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create("file:///etc/passwd", directory); err == nil {
		t.Fatal("expected non-http URL to be rejected")
	}
	if got := sanitizeFilename("../../bad:name?.zip"); got != "bad_name_.zip" {
		t.Fatalf("unexpected sanitized filename %q", got)
	}
	if got := totalFromContentRange("bytes 10-19/120"); got != 120 {
		t.Fatalf("unexpected content range total %d", got)
	}
	if got := totalFromContentRange("invalid"); got != -1 {
		t.Fatalf("unexpected invalid range result %d", got)
	}
	if strings.Contains(sanitizeFilename("safe.txt"), "/") {
		t.Fatal("safe filename contains path separator")
	}
}

func TestBrowseDirectories(t *testing.T) {
	directory := t.TempDir()
	child := filepath.Join(directory, "media")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "not-a-folder.txt"), []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(directory, filepath.Join(directory, ".state", "jobs.json"), 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	listing, err := manager.BrowseDirectories(directory)
	if err != nil {
		t.Fatal(err)
	}
	if listing.Path != directory || listing.DefaultPath != directory {
		t.Fatalf("unexpected listing paths: %#v", listing)
	}
	if len(listing.Directories) != 1 || listing.Directories[0].Path != child {
		t.Fatalf("unexpected directory entries: %#v", listing.Directories)
	}
	if _, err := manager.BrowseDirectories("relative/path"); err == nil {
		t.Fatal("expected relative path to be rejected")
	}
}

func TestCreateDirectory(t *testing.T) {
	directory := t.TempDir()
	manager, err := NewManager(directory, filepath.Join(directory, ".state", "jobs.json"), 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.CreateDirectory(directory, "New downloads")
	if err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(directory, "New downloads")
	if created.Name != "New downloads" || created.Path != expected {
		t.Fatalf("unexpected created directory: %#v", created)
	}
	if info, err := os.Stat(expected); err != nil || !info.IsDir() {
		t.Fatalf("created folder is missing: %v", err)
	}
	if _, err := manager.CreateDirectory(directory, "../outside"); err == nil {
		t.Fatal("expected a traversing folder name to be rejected")
	}
	if _, err := manager.CreateDirectory(directory, "New downloads"); err == nil {
		t.Fatal("expected a duplicate folder name to be rejected")
	}
}

func TestDeleteWaitsForTransferBeforeRemovingFile(t *testing.T) {
	directory := t.TempDir()
	manager, err := NewManager(directory, filepath.Join(directory, ".state", "jobs.json"), 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	body := &controlledBody{release: release}
	manager.client = &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        http.StatusText(http.StatusOK),
			Header:        make(http.Header),
			Body:          body,
			ContentLength: 64 * 1024,
			Request:       request,
		}, nil
	})}

	job, err := manager.Create("https://example.test/controlled.bin", directory)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		current, getErr := manager.Get(job.ID)
		return getErr == nil && current.DownloadedBytes >= 32*1024
	})

	deleted := make(chan error, 1)
	go func() { deleted <- manager.Delete(job.ID) }()
	select {
	case err := <-deleted:
		t.Fatalf("Delete returned before the transfer stopped: %v", err)
	case <-time.After(75 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Delete did not return after the transfer stopped")
	}
	if _, err := os.Stat(filepath.Join(directory, job.Filename)); !os.IsNotExist(err) {
		t.Fatalf("download file still exists after Delete: %v", err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not reached before timeout")
}
