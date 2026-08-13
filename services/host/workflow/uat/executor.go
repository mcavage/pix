package uat

import (
	"context"
	"io"
)

type ExecCmd interface {
	Run() error
	Output() ([]byte, error)
	StdoutPipe() (io.ReadCloser, error)
	StderrPipe() (io.ReadCloser, error)
}

type Exec interface {
	CommandContext(ctx context.Context, name string, args ...string) ExecCmd
}

type Sandbox interface {
	Create(ctx context.Context, runID string) error
	Probe(ctx context.Context, runID string) error
	Remove(ctx context.Context, runID string) error
}

type MCP interface {
	Add(ctx context.Context, name string, argv []string) error
	Auth(ctx context.Context, name string) error
	Status(ctx context.Context, name string) (string, error)
	Remove(ctx context.Context, name string) error
}

type Image interface {
	Load(ctx context.Context, tag, workspacePath string) error
	Probe(ctx context.Context, tag string) error
}

type Lease interface {
	Acquire(ctx context.Context, runID string, resource string) error
	Release(ctx context.Context, runID string, resource string) error
	Cleanup(ctx context.Context, runID string) error
}
