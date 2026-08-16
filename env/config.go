package env

import (
	"time"

	"github.com/criyle/go-judge/env/pool"
)

// WorkspaceBuilder creates a sandbox whose /w directory is backed by the
// supplied host directory. It is implemented on Linux only.
type WorkspaceBuilder interface {
	BuildWorkspace(path string) (pool.Environment, error)
}

// Config defines parameters to create environment builder
type Config struct {
	ContainerInitPath  string
	TmpFsParam         string
	NetShare           bool
	MountConf          string
	SeccompConf        string
	CgroupPrefix       string
	ContainerCredStart int
	EnableCPURate      bool
	CPUCfsPeriod       time.Duration
	NoFallback         bool
}
