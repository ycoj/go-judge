//go:build linux

package env

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/criyle/go-judge/env/linuxcontainer"
	"github.com/criyle/go-judge/env/pool"
	"github.com/criyle/go-sandbox/container"
	"github.com/criyle/go-sandbox/pkg/mount"
	"golang.org/x/sys/unix"
)

// workspaceEnvBuilder keeps the normal pooled builder while exposing a
// second construction path for persistent Session workspaces.
type workspaceEnvBuilder struct {
	normal     pool.EnvBuilder
	base       *container.Builder
	mounts     []mount.Mount
	cgroupPool linuxcontainer.CgroupPool
	workDir    string
	seccomp    []syscall.SockFilter
	cpuRate    bool
}

var _ pool.EnvBuilder = (*workspaceEnvBuilder)(nil)
var _ WorkspaceBuilder = (*workspaceEnvBuilder)(nil)

func (b *workspaceEnvBuilder) Build() (pool.Environment, error) {
	return b.normal.Build()
}

func (b *workspaceEnvBuilder) BuildWorkspace(path string) (pool.Environment, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("workspace: resolve path: %w", err)
	}
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		if err == nil {
			err = fmt.Errorf("not a directory")
		}
		return nil, fmt.Errorf("workspace: %s: %w", path, err)
	}

	cb := *b.base
	cb.CloneFlags |= unix.CLONE_NEWNET
	cb.Mounts = append([]mount.Mount(nil), b.mounts...)
	workspaceMounted := false
	for i := range cb.Mounts {
		target := strings.TrimPrefix(filepath.Clean(cb.Mounts[i].Target), "/")
		if target != "w" {
			continue
		}
		cb.Mounts[i] = mount.NewBuilder().WithBind(path, "w", false).Mounts[0]
		workspaceMounted = true
		break
	}
	if !workspaceMounted {
		cb.Mounts = append(cb.Mounts, mount.NewBuilder().WithBind(path, "w", false).Mounts[0])
	}

	builder := linuxcontainer.NewEnvBuilder(linuxcontainer.Config{
		Builder:    &cb,
		CgroupPool: b.cgroupPool,
		WorkDir:    b.workDir,
		CPURate:    b.cpuRate,
		Seccomp:    b.seccomp,
	})
	return builder.Build()
}

// workspaceBuilderFrom returns a workspace-capable wrapper without changing
// the existing normal environment pool behavior.
func workspaceBuilderFrom(normal pool.EnvBuilder, base *container.Builder, mounts []mount.Mount, cgroupPool linuxcontainer.CgroupPool, workDir string, seccomp []syscall.SockFilter, cpuRate bool) pool.EnvBuilder {
	return &workspaceEnvBuilder{
		normal:     normal,
		base:       base,
		mounts:     append([]mount.Mount(nil), mounts...),
		cgroupPool: cgroupPool,
		workDir:    workDir,
		seccomp:    seccomp,
		cpuRate:    cpuRate,
	}
}
