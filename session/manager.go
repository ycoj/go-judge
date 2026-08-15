package session

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/criyle/go-judge/env/pool"
	"github.com/criyle/go-judge/envexec"
)

const (
	DefaultTTL             = 30 * time.Minute
	DefaultMaxDiskBytes    = 1024 << 20
	DefaultDiskCheckPeriod = 50 * time.Millisecond
	reservedStdoutPrefix   = ".go-judge-session-stdout-"
	reservedStderrPrefix   = ".go-judge-session-stderr-"
)

var (
	ErrNotFound       = errors.New("session not found")
	ErrFileNotFound   = errors.New("file not found")
	ErrInvalidPath    = errors.New("invalid session path")
	ErrDiskLimit      = errors.New("session disk limit exceeded")
	ErrAlreadyClosed  = errors.New("session is closed")
	ErrUnsupported    = errors.New("session sandbox is not supported")
	ErrInvalidRequest = errors.New("invalid session request")
)

// WorkspaceBuilder is implemented by the Linux environment builder.
type WorkspaceBuilder interface {
	BuildWorkspace(path string) (pool.Environment, error)
}

type Config struct {
	Root              string
	Builder           WorkspaceBuilder
	DefaultTTL        time.Duration
	DefaultMaxDisk    int64
	DiskCheckInterval time.Duration
	OutputLimit       uint64
	ExtraMemoryLimit  envexec.Size
	OpenFileLimit     uint64
	Parallelism       int
}

type Manager struct {
	root              string
	builder           WorkspaceBuilder
	defaultTTL        time.Duration
	defaultMaxDisk    int64
	diskCheckInterval time.Duration
	outputLimit       uint64
	extraMemoryLimit  envexec.Size
	openFileLimit     uint64
	sem               chan struct{}

	mu        sync.RWMutex
	sessions  map[string]*Session
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

type Session struct {
	id        string
	createdAt time.Time
	workspace string
	ttl       time.Duration
	maxDisk   int64
	manager   *Manager

	mu       sync.Mutex
	refs     atomic.Int32
	closed   bool
	lastUsed atomic.Int64
	env      pool.Environment
}

type CreateRequest struct {
	TTL       int64 `json:"ttl"`
	MaxDiskMB int64 `json:"maxDiskMB"`
}

type FileEntry struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"modTime"`
}

type ExecRequest struct {
	Args        []string `json:"args"`
	Env         []string `json:"env,omitempty"`
	CPULimit    uint64   `json:"cpuLimit,omitempty"`
	ClockLimit  uint64   `json:"clockLimit,omitempty"`
	MemoryLimit uint64   `json:"memoryLimit,omitempty"`
	ProcLimit   uint64   `json:"procLimit,omitempty"`
	Stdin       string   `json:"stdin,omitempty"`
}

type ExecResponse struct {
	Status     string `json:"status"`
	ExitStatus int    `json:"exitStatus"`
	Time       uint64 `json:"time"`
	Memory     uint64 `json:"memory"`
	RunTime    uint64 `json:"runTime"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Error      string `json:"error"`
}

type ExecutionResult struct {
	Response ExecResponse
}

func NewManager(cfg Config) (*Manager, error) {
	if cfg.Builder == nil {
		return nil, ErrUnsupported
	}
	if cfg.Root == "" {
		return nil, fmt.Errorf("session root is empty")
	}
	if cfg.DefaultTTL <= 0 {
		cfg.DefaultTTL = DefaultTTL
	}
	if cfg.DefaultMaxDisk <= 0 {
		cfg.DefaultMaxDisk = DefaultMaxDiskBytes
	}
	if cfg.DiskCheckInterval <= 0 {
		cfg.DiskCheckInterval = DefaultDiskCheckPeriod
	}
	if cfg.OutputLimit == 0 {
		cfg.OutputLimit = 64 << 20
	}
	if cfg.Parallelism <= 0 {
		cfg.Parallelism = 1
	}
	if err := os.MkdirAll(cfg.Root, 0o700); err != nil {
		return nil, fmt.Errorf("create session root: %w", err)
	}
	m := &Manager{
		root:              cfg.Root,
		builder:           cfg.Builder,
		defaultTTL:        cfg.DefaultTTL,
		defaultMaxDisk:    cfg.DefaultMaxDisk,
		diskCheckInterval: cfg.DiskCheckInterval,
		outputLimit:       cfg.OutputLimit,
		extraMemoryLimit:  cfg.ExtraMemoryLimit,
		openFileLimit:     cfg.OpenFileLimit,
		sem:               make(chan struct{}, cfg.Parallelism),
		sessions:          make(map[string]*Session),
		stop:              make(chan struct{}),
		done:              make(chan struct{}),
	}
	go m.gcLoop()
	return m, nil
}

func (m *Manager) Create(req CreateRequest) (*Session, error) {
	ttl := m.defaultTTL
	if req.TTL > 0 {
		if req.TTL > math.MaxInt64/int64(time.Second) {
			return nil, fmt.Errorf("%w: ttl is too large", ErrInvalidRequest)
		}
		ttl = time.Duration(req.TTL) * time.Second
	} else if req.TTL < 0 {
		return nil, fmt.Errorf("%w: ttl must be non-negative", ErrInvalidRequest)
	}
	maxDisk := m.defaultMaxDisk
	if req.MaxDiskMB > 0 {
		if req.MaxDiskMB > math.MaxInt64/(1024*1024) {
			return nil, fmt.Errorf("%w: maxDiskMB is too large", ErrInvalidRequest)
		}
		maxDisk = req.MaxDiskMB * 1024 * 1024
	} else if req.MaxDiskMB < 0 {
		return nil, fmt.Errorf("%w: maxDiskMB must be non-negative", ErrInvalidRequest)
	}

	for i := 0; i < 10; i++ {
		id, err := newID()
		if err != nil {
			return nil, err
		}
		workspace := filepath.Join(m.root, id)
		if err := os.Mkdir(workspace, 0o777); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return nil, fmt.Errorf("create session workspace: %w", err)
		}
		now := time.Now()
		s := &Session{id: id, createdAt: now, workspace: workspace, ttl: ttl, maxDisk: maxDisk, manager: m}
		s.lastUsed.Store(now.UnixNano())
		m.mu.Lock()
		m.sessions[id] = s
		m.mu.Unlock()
		return s, nil
	}
	return nil, fmt.Errorf("could not generate session id")
}

func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sess_" + hex.EncodeToString(b), nil
}

func (m *Manager) Get(id string) (*Session, error) {
	m.mu.RLock()
	s := m.sessions[id]
	m.mu.RUnlock()
	if s == nil {
		return nil, ErrNotFound
	}
	if err := s.begin(); err != nil {
		return nil, err
	}
	s.end()
	return s, nil
}

func (s *Session) begin() error {
	s.refs.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		s.refs.Add(-1)
		return ErrNotFound
	}
	s.lastUsed.Store(time.Now().UnixNano())
	return nil
}

func (s *Session) end() { s.refs.Add(-1) }

func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	s := m.sessions[id]
	if s == nil {
		m.mu.Unlock()
		return ErrNotFound
	}
	delete(m.sessions, id)
	m.mu.Unlock()

	s.mu.Lock()
	s.closed = true
	if s.env != nil {
		_ = s.env.Destroy()
		s.env = nil
	}
	s.mu.Unlock()
	return os.RemoveAll(s.workspace)
}

func (s *Session) ID() string        { return s.id }
func (s *Session) CreatedAt() int64  { return s.createdAt.Unix() }
func (s *Session) Workspace() string { return s.workspace }

func (s *Session) withLock(fn func() error) error {
	if err := s.begin(); err != nil {
		return err
	}
	defer s.end()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrNotFound
	}
	s.lastUsed.Store(time.Now().UnixNano())
	return fn()
}

func (s *Session) WriteFile(name string, r io.Reader) (int64, error) {
	var size int64
	err := s.withLock(func() error {
		p, err := s.resolve(name, true)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
			return err
		}
		oldSize := int64(0)
		if fi, statErr := os.Stat(p); statErr == nil && fi.Mode().IsRegular() {
			oldSize = fi.Size()
		}
		usage, err := s.usage()
		if err != nil {
			return err
		}
		remaining := s.maxDisk - (usage - oldSize)
		if remaining < 0 {
			return ErrDiskLimit
		}
		tmp, err := os.CreateTemp(filepath.Dir(p), ".session-write-*")
		if err != nil {
			return err
		}
		tmpName := tmp.Name()
		defer os.Remove(tmpName)
		written, copyErr := io.Copy(tmp, io.LimitReader(r, remaining+1))
		if copyErr != nil {
			tmp.Close()
			return copyErr
		}
		if written > remaining {
			tmp.Close()
			return ErrDiskLimit
		}
		if err := tmp.Chmod(0o666); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		if err := os.Rename(tmpName, p); err != nil {
			return err
		}
		size = written
		return nil
	})
	return size, err
}

func (s *Session) ReadFile(name string) ([]byte, error) {
	var data []byte
	err := s.withLock(func() error {
		p, err := s.resolve(name, false)
		if err != nil {
			return err
		}
		data, err = os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			return ErrFileNotFound
		}
		return err
	})
	return data, err
}

func (s *Session) ListFiles() ([]FileEntry, error) {
	var files []FileEntry
	err := s.withLock(func() error {
		return filepath.WalkDir(s.workspace, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			fi, err := d.Info()
			if err != nil || !fi.Mode().IsRegular() {
				return err
			}
			rel, err := filepath.Rel(s.workspace, p)
			if err != nil {
				return err
			}
			files = append(files, FileEntry{Name: filepath.ToSlash(rel), Size: fi.Size(), ModTime: fi.ModTime().Unix()})
			return nil
		})
	})
	return files, err
}

func (s *Session) Archive(patterns string) (string, error) {
	var archivePath string
	err := s.withLock(func() error {
		selected, err := s.selectFiles(patterns)
		if err != nil {
			return err
		}
		f, err := os.CreateTemp("", "go-judge-session-*.zip")
		if err != nil {
			return err
		}
		archivePath = f.Name()
		zw := zip.NewWriter(f)
		for _, p := range selected {
			rel, _ := filepath.Rel(s.workspace, p)
			name := filepath.ToSlash(rel)
			entry, err := zw.Create(name)
			if err == nil {
				in, openErr := os.Open(p)
				if openErr != nil {
					err = openErr
				} else {
					_, err = io.Copy(entry, in)
					in.Close()
				}
			}
			if err != nil {
				zw.Close()
				f.Close()
				os.Remove(archivePath)
				archivePath = ""
				return err
			}
		}
		if err := zw.Close(); err != nil {
			f.Close()
			os.Remove(archivePath)
			archivePath = ""
			return err
		}
		return f.Close()
	})
	return archivePath, err
}

func (s *Session) selectFiles(patterns string) ([]string, error) {
	var globs []string
	for _, raw := range strings.Split(patterns, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if filepath.IsAbs(raw) || strings.Contains(raw, "..") {
			return nil, ErrInvalidPath
		}
		globs = append(globs, filepath.ToSlash(raw))
	}
	var files []string
	err := filepath.WalkDir(s.workspace, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		fi, err := d.Info()
		if err != nil || !fi.Mode().IsRegular() {
			return err
		}
		rel, _ := filepath.Rel(s.workspace, p)
		rel = filepath.ToSlash(rel)
		if len(globs) == 0 {
			files = append(files, p)
			return nil
		}
		for _, glob := range globs {
			matched, matchErr := path.Match(glob, rel)
			if matchErr != nil {
				return fmt.Errorf("%w: invalid archive pattern: %v", ErrInvalidPath, matchErr)
			}
			if matched {
				files = append(files, p)
				break
			}
		}
		return nil
	})
	return files, err
}

func (s *Session) Exec(ctx context.Context, req ExecRequest) (ExecResponse, error) {
	if len(req.Args) == 0 {
		return ExecResponse{}, ErrInvalidRequest
	}
	if req.CPULimit > math.MaxInt64 || req.ClockLimit > math.MaxInt64 {
		return ExecResponse{}, fmt.Errorf("%w: time limit is too large", ErrInvalidRequest)
	}
	var response ExecResponse
	err := s.withLock(func() error {
		if usage, usageErr := s.usage(); usageErr != nil {
			return usageErr
		} else if usage > s.maxDisk {
			return ErrDiskLimit
		}
		select {
		case s.manager.sem <- struct{}{}:
			defer func() { <-s.manager.sem }()
		case <-ctx.Done():
			return ctx.Err()
		}
		if s.env == nil {
			var err error
			s.env, err = s.manager.builder.BuildWorkspace(s.workspace)
			if err != nil {
				return fmt.Errorf("build session sandbox: %w", err)
			}
		}

		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		stdoutName := reservedStdoutPrefix + suffix
		stderrName := reservedStderrPrefix + suffix
		files := []envexec.File{
			envexec.NewFileReader(strings.NewReader(req.Stdin)),
			envexec.NewFileCollector(stdoutName, envexec.Size(s.manager.outputLimit), false),
			envexec.NewFileCollector(stderrName, envexec.Size(s.manager.outputLimit), false),
		}
		var quotaHit atomic.Bool
		clockLimit := time.Duration(req.ClockLimit)
		cpuLimit := time.Duration(req.CPULimit)
		wait := func(waitCtx context.Context, process envexec.Process) bool {
			limit := clockLimit
			if limit == 0 {
				limit = cpuLimit
			}
			ticker := time.NewTicker(s.manager.diskCheckInterval)
			defer ticker.Stop()
			start := time.Now()
			for {
				select {
				case <-waitCtx.Done():
					return false
				case <-process.Done():
					return false
				case <-ticker.C:
					if usage, usageErr := s.usage(); usageErr == nil && usage > s.maxDisk {
						quotaHit.Store(true)
						return true
					}
					if time.Since(start) > limit {
						return true
					}
					u := process.Usage()
					if cpuLimit > 0 && u.Time > cpuLimit {
						return true
					}
				}
			}
		}
		cmd := &envexec.Cmd{
			Environment:      s.env,
			Args:             req.Args,
			Env:              req.Env,
			Files:            files,
			TimeLimit:        cpuLimit,
			MemoryLimit:      envexec.Size(req.MemoryLimit),
			ExtraMemoryLimit: s.manager.extraMemoryLimit,
			OutputLimit:      envexec.Size(s.manager.outputLimit),
			ProcLimit:        req.ProcLimit,
			OpenFileLimit:    s.manager.openFileLimit,
			Waiter:           wait,
		}
		storeDir, err := os.MkdirTemp("", "go-judge-session-output-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(storeDir)
		result, runErr := (&envexec.Single{Cmd: cmd, NewStoreFile: func() (*os.File, error) {
			return os.CreateTemp(storeDir, "output-*")
		}}).Run(ctx)
		_ = os.Remove(filepath.Join(s.workspace, stdoutName))
		_ = os.Remove(filepath.Join(s.workspace, stderrName))
		if runErr != nil {
			return runErr
		}
		response = ExecResponse{
			Status:     result.Status.String(),
			ExitStatus: result.ExitStatus,
			Time:       uint64(result.Time),
			Memory:     uint64(result.Memory),
			RunTime:    uint64(result.RunTime),
		}
		for name, file := range result.Files {
			data, readErr := os.ReadFile(file.Name())
			file.Close()
			os.Remove(file.Name())
			if readErr != nil {
				return readErr
			}
			switch name {
			case stdoutName:
				response.Stdout = string(data)
			case stderrName:
				response.Stderr = string(data)
			}
		}
		if result.Error != "" {
			response.Error = result.Error
		}
		if quotaHit.Load() {
			response.Status = "Internal Error"
			response.Error = ErrDiskLimit.Error()
		}
		return nil
	})
	return response, err
}

func (s *Session) resolve(name string, forWrite bool) (string, error) {
	if name == "" || strings.IndexByte(name, 0) >= 0 || filepath.IsAbs(name) {
		return "", ErrInvalidPath
	}
	raw := filepath.FromSlash(name)
	for _, part := range strings.Split(raw, string(filepath.Separator)) {
		if part == ".." {
			return "", ErrInvalidPath
		}
	}
	clean := filepath.Clean(raw)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", ErrInvalidPath
	}
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if part == ".." {
			return "", ErrInvalidPath
		}
	}
	p := filepath.Join(s.workspace, clean)
	if err := ensureWithin(s.workspace, p, forWrite); err != nil {
		return "", err
	}
	return p, nil
}

func ensureWithin(root, target string, forWrite bool) error {
	root, _ = filepath.Abs(root)
	target, _ = filepath.Abs(target)
	check := target
	if forWrite {
		check = filepath.Dir(target)
	}
	resolved, suffix, err := resolveExistingAncestor(check)
	if err != nil {
		return ErrInvalidPath
	}
	if suffix != "" {
		resolved = filepath.Join(resolved, suffix)
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return ErrInvalidPath
	}
	return nil
}

func resolveExistingAncestor(path string) (resolved, suffix string, err error) {
	current := path
	var missing []string
	for {
		if _, statErr := os.Lstat(current); statErr == nil {
			resolved, err = filepath.EvalSymlinks(current)
			if err != nil {
				return "", "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, "", nil
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", "", statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", "", os.ErrNotExist
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func (s *Session) usage() (int64, error) {
	var total int64
	err := filepath.WalkDir(s.workspace, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if strings.HasPrefix(d.Name(), reservedStdoutPrefix) || strings.HasPrefix(d.Name(), reservedStderrPrefix) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			total += fi.Size()
		}
		return nil
	})
	return total, err
}

func (m *Manager) gcLoop() {
	interval := time.Second
	if m.defaultTTL/2 < interval {
		interval = m.defaultTTL / 2
	}
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer close(m.done)
	for {
		select {
		case <-ticker.C:
			m.collectExpired()
		case <-m.stop:
			return
		}
	}
}

func (m *Manager) collectExpired() {
	now := time.Now()
	m.mu.RLock()
	var expired []string
	for id, s := range m.sessions {
		last := time.Unix(0, s.lastUsed.Load())
		if s.refs.Load() == 0 && now.Sub(last) >= s.ttl {
			expired = append(expired, id)
		}
	}
	m.mu.RUnlock()
	for _, id := range expired {
		_ = m.Delete(id)
	}
}

func (m *Manager) Close() error {
	var closeErr error
	m.closeOnce.Do(func() {
		close(m.stop)
		<-m.done
		m.mu.Lock()
		ids := make([]string, 0, len(m.sessions))
		for id := range m.sessions {
			ids = append(ids, id)
		}
		m.mu.Unlock()
		for _, id := range ids {
			if err := m.Delete(id); err != nil && !errors.Is(err, ErrNotFound) && closeErr == nil {
				closeErr = err
			}
		}
	})
	return closeErr
}
