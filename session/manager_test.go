package session

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/criyle/go-judge/env/pool"
	"github.com/criyle/go-judge/envexec"
)

type fakeBuilder struct{}

func (fakeBuilder) BuildWorkspace(string) (pool.Environment, error) { return &fakeEnvironment{}, nil }

type fakeEnvironment struct{}

func (*fakeEnvironment) Execve(context.Context, envexec.ExecveParam) (envexec.Process, error) {
	return nil, errors.New("not implemented")
}
func (*fakeEnvironment) Open([]envexec.OpenParam) ([]envexec.OpenResult, error) { return nil, nil }
func (*fakeEnvironment) Symlink([]envexec.SymlinkParam) ([]error, error)        { return nil, nil }
func (*fakeEnvironment) Reset() error                                           { return nil }
func (*fakeEnvironment) Destroy() error                                         { return nil }

func newTestManager(t *testing.T, maxDisk int64) *Manager {
	t.Helper()
	m, err := NewManager(Config{
		Root:              t.TempDir(),
		Builder:           fakeBuilder{},
		DefaultMaxDisk:    maxDisk,
		DefaultTTL:        time.Hour,
		DiskCheckInterval: 10 * time.Millisecond,
		Parallelism:       1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestSessionFileLifecycleAndPathGuards(t *testing.T) {
	m := newTestManager(t, 1024)
	s, err := m.Create(CreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteFile("../escape", stringsReader("bad")); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("expected traversal rejection, got %v", err)
	}
	if _, err := s.WriteFile("nested/code.py", stringsReader("print(1)")); err != nil {
		t.Fatal(err)
	}
	data, err := s.ReadFile("nested/code.py")
	if err != nil || string(data) != "print(1)" {
		t.Fatalf("read mismatch: %q, %v", data, err)
	}
	files, err := s.ListFiles()
	if err != nil || len(files) != 1 || files[0].Name != "nested/code.py" {
		t.Fatalf("unexpected file list: %+v, %v", files, err)
	}
	if _, err := s.ReadFile("missing"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("expected missing file error, got %v", err)
	}
}

func TestSessionDiskLimitAndArchivePattern(t *testing.T) {
	m := newTestManager(t, 5)
	s, err := m.Create(CreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteFile("1.in", stringsReader("12345")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteFile("2.out", stringsReader("x")); !errors.Is(err, ErrDiskLimit) {
		t.Fatalf("expected disk limit, got %v", err)
	}
	archivePath, err := s.Archive("*.in")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(archivePath)
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if len(r.File) != 1 || r.File[0].Name != "1.in" {
		t.Fatalf("unexpected archive entries: %+v", r.File)
	}
}

func TestSessionTTLGC(t *testing.T) {
	m, err := NewManager(Config{
		Root:              t.TempDir(),
		Builder:           fakeBuilder{},
		DefaultTTL:        time.Second,
		DefaultMaxDisk:    1024,
		DiskCheckInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.Create(CreateRequest{TTL: 1})
	if err != nil {
		t.Fatal(err)
	}
	workspace := s.Workspace()
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(workspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected workspace to be collected, stat=%v", err)
	}
	_ = m.Close()
}

func stringsReader(s string) io.Reader { return strings.NewReader(s) }
