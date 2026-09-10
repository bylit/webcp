package download

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Status string

const (
	StatusQueued      Status = "queued"
	StatusDownloading Status = "downloading"
	StatusPaused      Status = "paused"
	StatusCompleted   Status = "completed"
	StatusError       Status = "error"
)

type Job struct {
	ID                  string    `json:"id"`
	URL                 string    `json:"url"`
	Filename            string    `json:"filename"`
	Destination         string    `json:"destination"`
	Status              Status    `json:"status"`
	DownloadedBytes     int64     `json:"downloaded_bytes"`
	TotalBytes          int64     `json:"total_bytes"`
	SpeedBytesPerSecond int64     `json:"speed_bytes_per_second"`
	Error               string    `json:"error,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`

	cancel  context.CancelFunc `json:"-"`
	deleted bool               `json:"-"`
	runtime *jobRuntime        `json:"-"`
}

type jobRuntime struct {
	workers sync.WaitGroup
}

type Directory struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type DirectoryListing struct {
	Path        string      `json:"path"`
	Parent      string      `json:"parent"`
	DefaultPath string      `json:"default_path"`
	Directories []Directory `json:"directories"`
}

type Manager struct {
	mu            sync.RWMutex
	jobs          map[string]*Job
	defaultPath   string
	stateFile     string
	client        *http.Client
	semaphore     chan struct{}
	logger        *slog.Logger
	persistSignal chan struct{}
}

var (
	ErrNotFound     = errors.New("download not found")
	ErrInvalidState = errors.New("download is not in a valid state for this action")
)

func NewManager(defaultPath, stateFile string, maxConcurrent int, logger *slog.Logger) (*Manager, error) {
	defaultPath = strings.TrimSpace(defaultPath)
	if defaultPath == "" {
		return nil, errors.New("default download path is required")
	}
	absoluteDefault, err := filepath.Abs(defaultPath)
	if err != nil {
		return nil, fmt.Errorf("resolve default download path: %w", err)
	}
	defaultPath = filepath.Clean(absoluteDefault)
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	if logger == nil {
		logger = slog.Default()
	}
	m := &Manager{
		jobs:          make(map[string]*Job),
		defaultPath:   defaultPath,
		stateFile:     stateFile,
		client:        &http.Client{Timeout: 0},
		semaphore:     make(chan struct{}, maxConcurrent),
		logger:        logger,
		persistSignal: make(chan struct{}, 1),
	}
	if err := os.MkdirAll(defaultPath, 0o755); err != nil {
		return nil, fmt.Errorf("create default download directory: %w", err)
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	go m.persistenceLoop()
	return m, nil
}

func (m *Manager) DefaultPath() string {
	return m.defaultPath
}

func (m *Manager) BrowseDirectories(requestedPath string) (DirectoryListing, error) {
	path, err := m.resolveDestination(requestedPath, false)
	if err != nil {
		return DirectoryListing{}, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return DirectoryListing{}, fmt.Errorf("cannot read %s: %w", path, err)
	}
	directories := make([]Directory, 0)
	for _, entry := range entries {
		isDirectory := entry.IsDir()
		if !isDirectory && entry.Type()&os.ModeSymlink != 0 {
			if info, statErr := os.Stat(filepath.Join(path, entry.Name())); statErr == nil {
				isDirectory = info.IsDir()
			}
		}
		if isDirectory {
			directories = append(directories, Directory{
				Name: entry.Name(),
				Path: filepath.Join(path, entry.Name()),
			})
		}
	}
	return DirectoryListing{
		Path:        path,
		Parent:      filepath.Dir(path),
		DefaultPath: m.defaultPath,
		Directories: directories,
	}, nil
}

func (m *Manager) CreateDirectory(parentPath, name string) (Directory, error) {
	parent, err := m.resolveDestination(parentPath, false)
	if err != nil {
		return Directory{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
		return Directory{}, errors.New("folder name must be a single directory name")
	}
	if len([]byte(name)) > 255 {
		return Directory{}, errors.New("folder name is too long")
	}
	for _, character := range name {
		if character < 32 {
			return Directory{}, errors.New("folder name contains unsupported characters")
		}
	}

	path := filepath.Join(parent, name)
	if err := os.Mkdir(path, 0o755); err != nil {
		if os.IsExist(err) {
			return Directory{}, errors.New("a folder with that name already exists")
		}
		return Directory{}, fmt.Errorf("create folder: %w", err)
	}
	if _, err := m.resolveDestination(path, true); err != nil {
		_ = os.Remove(path)
		return Directory{}, err
	}
	return Directory{Name: name, Path: path}, nil
}

func (m *Manager) Recover() {
	m.mu.Lock()
	var resume []*Job
	for _, job := range m.jobs {
		if job.Destination == "" {
			// Jobs written by versions before filesystem browsing used the default storage.
			job.Destination = m.defaultPath
		}
		info, err := os.Stat(filepath.Join(job.Destination, job.Filename))
		if err == nil {
			job.DownloadedBytes = info.Size()
		} else if !os.IsNotExist(err) {
			job.Status = StatusError
			job.Error = err.Error()
			continue
		} else if job.Status == StatusCompleted {
			job.Status = StatusError
			job.Error = "downloaded file is missing"
			continue
		} else {
			job.DownloadedBytes = 0
		}
		if job.Status == StatusDownloading || job.Status == StatusQueued {
			job.Status = StatusQueued
			resume = append(resume, job)
		}
	}
	m.persistLocked()
	for _, job := range resume {
		m.startLocked(job)
	}
	m.mu.Unlock()
}

func (m *Manager) Create(rawURL, destination string) (Job, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return Job{}, errors.New("enter a valid http or https URL")
	}
	destination, err = m.resolveDestination(destination, true)
	if err != nil {
		return Job{}, err
	}

	now := time.Now().UTC()
	job := &Job{
		ID:          newID(),
		URL:         parsed.String(),
		Filename:    filenameFromURL(parsed),
		Destination: destination,
		Status:      StatusQueued,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	m.mu.Lock()
	job.Filename = m.availableFilenameLocked(destination, job.Filename, "")
	m.jobs[job.ID] = job
	m.persistLocked()
	m.startLocked(job)
	copy := *job
	m.mu.Unlock()
	return copy, nil
}

func (m *Manager) List() []Job {
	m.mu.RLock()
	result := make([]Job, 0, len(m.jobs))
	for _, job := range m.jobs {
		result = append(result, cloneJob(job))
	}
	m.mu.RUnlock()
	slices.SortFunc(result, func(a, b Job) int {
		return b.CreatedAt.Compare(a.CreatedAt)
	})
	return result
}

func (m *Manager) Get(id string) (Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job, ok := m.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	return cloneJob(job), nil
}

func (m *Manager) Pause(id string) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	if job.Status != StatusDownloading && job.Status != StatusQueued {
		return Job{}, ErrInvalidState
	}
	job.Status = StatusPaused
	job.SpeedBytesPerSecond = 0
	job.UpdatedAt = time.Now().UTC()
	if job.cancel != nil {
		job.cancel()
		job.cancel = nil
	}
	m.persistLocked()
	return cloneJob(job), nil
}

func (m *Manager) Resume(id string) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	if job.Status != StatusPaused && job.Status != StatusError {
		return Job{}, ErrInvalidState
	}
	job.Status = StatusQueued
	job.Error = ""
	job.UpdatedAt = time.Now().UTC()
	m.persistLocked()
	m.startLocked(job)
	return cloneJob(job), nil
}

func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	job.deleted = true
	if job.cancel != nil {
		job.cancel()
	}
	delete(m.jobs, id)
	path := filepath.Join(job.Destination, job.Filename)
	runtime := job.runtime
	m.persistLocked()
	m.mu.Unlock()

	// Cancellation is only a signal. Wait for all current and recently cancelled
	// generations to close their file handles before unlinking the destination.
	if runtime != nil {
		runtime.workers.Wait()
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove file: %w", err)
	}
	return nil
}

func (m *Manager) File(id string) (string, Job, error) {
	job, err := m.Get(id)
	if err != nil {
		return "", Job{}, err
	}
	if job.Status != StatusCompleted {
		return "", Job{}, ErrInvalidState
	}
	path := filepath.Join(job.Destination, job.Filename)
	if _, err := os.Stat(path); err != nil {
		return "", Job{}, err
	}
	return path, job, nil
}

func (m *Manager) PauseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, job := range m.jobs {
		if job.Status == StatusDownloading || job.Status == StatusQueued {
			job.Status = StatusPaused
			job.SpeedBytesPerSecond = 0
			if job.cancel != nil {
				job.cancel()
				job.cancel = nil
			}
		}
	}
	m.persistLocked()
}

func (m *Manager) startLocked(job *Job) {
	if job.runtime == nil {
		job.runtime = &jobRuntime{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	job.cancel = cancel
	job.runtime.workers.Add(1)
	go m.run(ctx, job)
}

func (m *Manager) run(ctx context.Context, job *Job) {
	defer job.runtime.workers.Done()
	select {
	case m.semaphore <- struct{}{}:
		defer func() { <-m.semaphore }()
	case <-ctx.Done():
		return
	}

	m.mu.Lock()
	if job.deleted || job.Status != StatusQueued {
		m.mu.Unlock()
		return
	}
	job.Status = StatusDownloading
	job.UpdatedAt = time.Now().UTC()
	m.mu.Unlock()

	if err := m.transfer(ctx, job); err != nil {
		if ctx.Err() != nil {
			return
		}
		m.mu.Lock()
		if !job.deleted {
			job.Status = StatusError
			job.Error = err.Error()
			job.SpeedBytesPerSecond = 0
			job.UpdatedAt = time.Now().UTC()
			m.persistLocked()
		}
		m.mu.Unlock()
		m.logger.Warn("download failed", "id", job.ID, "error", err)
		return
	}

	m.mu.Lock()
	if !job.deleted && job.Status == StatusDownloading {
		job.Status = StatusCompleted
		job.SpeedBytesPerSecond = 0
		job.DownloadedBytes = max(job.DownloadedBytes, job.TotalBytes)
		job.UpdatedAt = time.Now().UTC()
		job.cancel = nil
		m.persistLocked()
	}
	m.mu.Unlock()
}

func (m *Manager) transfer(ctx context.Context, job *Job) error {
	m.mu.RLock()
	filename := job.Filename
	destination := job.Destination
	m.mu.RUnlock()
	path := filepath.Join(destination, filename)
	offset := int64(0)
	if info, err := os.Stat(path); err == nil {
		offset = info.Size()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, job.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "webcp/1.0")
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := ctx.Err(); err != nil {
		return err
	}

	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && offset > 0 {
		if total := totalFromContentRange(resp.Header.Get("Content-Range")); total == offset {
			m.mu.Lock()
			job.TotalBytes = total
			job.DownloadedBytes = total
			m.mu.Unlock()
			return nil
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("remote server returned %s", resp.Status)
	}

	if offset > 0 && resp.StatusCode != http.StatusPartialContent {
		offset = 0 // Server ignored Range, so safely restart instead of appending duplicates.
	}

	if offset == 0 {
		if remoteName := filenameFromResponse(resp); remoteName != "" && remoteName != filename {
			m.mu.Lock()
			if job.deleted {
				m.mu.Unlock()
				return context.Canceled
			}
			newName := m.availableFilenameLocked(destination, remoteName, job.ID)
			job.Filename = newName
			filename = newName
			path = filepath.Join(destination, filename)
			m.persistLocked()
			m.mu.Unlock()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	flags := os.O_CREATE | os.O_WRONLY
	if offset == 0 {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_APPEND
	}
	file, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()

	total := resp.ContentLength
	if resp.StatusCode == http.StatusPartialContent {
		if value := totalFromContentRange(resp.Header.Get("Content-Range")); value >= 0 {
			total = value
		} else if total >= 0 {
			total += offset
		}
	}
	m.mu.Lock()
	job.DownloadedBytes = offset
	job.TotalBytes = total
	job.Error = ""
	m.mu.Unlock()

	buffer := make([]byte, 128*1024)
	downloaded := offset
	windowBytes := int64(0)
	windowStart := time.Now()
	lastPersist := windowStart
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			if _, err := file.Write(buffer[:n]); err != nil {
				return err
			}
			downloaded += int64(n)
			windowBytes += int64(n)
			now := time.Now()
			elapsed := now.Sub(windowStart)
			m.mu.Lock()
			job.DownloadedBytes = downloaded
			job.UpdatedAt = now.UTC()
			if elapsed >= 400*time.Millisecond {
				job.SpeedBytesPerSecond = int64(float64(windowBytes) / elapsed.Seconds())
				windowBytes = 0
				windowStart = now
			}
			if now.Sub(lastPersist) >= 2*time.Second {
				m.signalPersist()
				lastPersist = now
			}
			m.mu.Unlock()
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func (m *Manager) availableFilenameLocked(destination, requested, exceptID string) string {
	requested = sanitizeFilename(requested)
	ext := filepath.Ext(requested)
	base := strings.TrimSuffix(requested, ext)
	candidate := requested
	for index := 1; ; index++ {
		used := false
		if _, err := os.Stat(filepath.Join(destination, candidate)); err == nil {
			used = true
		}
		for _, existing := range m.jobs {
			if existing.ID != exceptID && existing.Destination == destination && existing.Filename == candidate {
				used = true
				break
			}
		}
		if !used {
			return candidate
		}
		candidate = fmt.Sprintf("%s (%d)%s", base, index, ext)
	}
}

func (m *Manager) resolveDestination(requestedPath string, requireWritable bool) (string, error) {
	requestedPath = strings.TrimSpace(requestedPath)
	if requestedPath == "" {
		requestedPath = m.defaultPath
	}
	if !filepath.IsAbs(requestedPath) {
		return "", errors.New("destination must be an absolute container path")
	}
	path := filepath.Clean(requestedPath)
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("destination is not available: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("destination must be a directory")
	}
	if requireWritable {
		probe, err := os.CreateTemp(path, ".webcp-write-check-*")
		if err != nil {
			return "", fmt.Errorf("destination is not writable: %w", err)
		}
		probeName := probe.Name()
		if closeErr := probe.Close(); closeErr != nil {
			_ = os.Remove(probeName)
			return "", fmt.Errorf("destination is not writable: %w", closeErr)
		}
		if err := os.Remove(probeName); err != nil {
			return "", fmt.Errorf("could not verify destination: %w", err)
		}
	}
	return path, nil
}

func (m *Manager) load() error {
	data, err := os.ReadFile(m.stateFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	var jobs []Job
	if err := json.Unmarshal(data, &jobs); err != nil {
		return fmt.Errorf("decode state: %w", err)
	}
	for i := range jobs {
		job := jobs[i]
		if job.ID == "" || job.URL == "" || sanitizeFilename(job.Filename) != job.Filename {
			m.logger.Warn("ignoring invalid persisted download")
			continue
		}
		if job.Destination != "" && (!filepath.IsAbs(job.Destination) || filepath.Clean(job.Destination) != job.Destination) {
			m.logger.Warn("ignoring download with invalid destination", "id", job.ID)
			continue
		}
		job.cancel = nil
		m.jobs[job.ID] = &job
	}
	return nil
}

func (m *Manager) persistenceLoop() {
	for range m.persistSignal {
		m.mu.Lock()
		m.persistLocked()
		m.mu.Unlock()
	}
}

func (m *Manager) signalPersist() {
	select {
	case m.persistSignal <- struct{}{}:
	default:
	}
}

func (m *Manager) persistLocked() {
	jobs := make([]Job, 0, len(m.jobs))
	for _, job := range m.jobs {
		jobs = append(jobs, cloneJob(job))
	}
	data, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		m.logger.Error("could not encode state", "error", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.stateFile), 0o755); err != nil {
		m.logger.Error("could not create state directory", "error", err)
		return
	}
	temporary := m.stateFile + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		m.logger.Error("could not write state", "error", err)
		return
	}
	if err := os.Rename(temporary, m.stateFile); err != nil {
		m.logger.Error("could not save state", "error", err)
	}
}

func cloneJob(job *Job) Job {
	copy := *job
	copy.cancel = nil
	copy.deleted = false
	copy.runtime = nil
	return copy
}

func filenameFromURL(parsed *url.URL) string {
	name, err := url.PathUnescape(filepath.Base(parsed.Path))
	if err != nil || name == "" || name == "." || name == "/" {
		return "download"
	}
	return sanitizeFilename(name)
}

func filenameFromResponse(resp *http.Response) string {
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	if err == nil && params["filename"] != "" {
		return sanitizeFilename(params["filename"])
	}
	return ""
}

func sanitizeFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, ". ")
	if name == "" {
		return "download"
	}
	runes := []rune(name)
	if len(runes) > 180 {
		name = string(runes[:180])
	}
	return name
}

func totalFromContentRange(value string) int64 {
	parts := strings.Split(value, "/")
	if len(parts) != 2 || parts[1] == "*" {
		return -1
	}
	total, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return -1
	}
	return total
}

func newID() string {
	value := make([]byte, 8)
	if _, err := rand.Read(value); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(value)
}
