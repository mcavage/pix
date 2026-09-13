package hosttools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	outputLimit     = 64 * 1024
	jobHistoryLimit = 64
	runningJobLimit = 4
)

type Service struct {
	Config Config
	mu     sync.Mutex
	jobs   map[string]*job
	order  []string
	closed bool
	wg     sync.WaitGroup
}
type job struct {
	mu        sync.Mutex
	output    []byte
	truncated bool
	done      bool
	exit      int
	cancel    context.CancelFunc
}

func (j *job) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	n := len(p)
	space := outputLimit - len(j.output)
	if len(p) > space {
		p = p[:space]
		j.truncated = true
	}
	j.output = append(j.output, p...)
	return n, nil
}
func (s *Service) start(argv []string, cwd string, timeout time.Duration, trial bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errors.New("host tools session has ended")
	}
	running := 0
	for _, j := range s.jobs {
		j.mu.Lock()
		if !j.done {
			running++
		}
		j.mu.Unlock()
	}
	if running >= runningJobLimit {
		return "", errors.New("four host jobs are already running; wait or cancel one")
	}
	// Keep job output bounded without turning the retained history bound into a
	// lifetime execution quota. Once the history is full, forget the oldest
	// completed job before accepting another one. Running jobs are never evicted.
	if len(s.jobs) >= jobHistoryLimit {
		for i, id := range s.order {
			j := s.jobs[id]
			if j == nil {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
			j.mu.Lock()
			done := j.done
			j.mu.Unlock()
			if done {
				delete(s.jobs, id)
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
	}
	if len(s.jobs) >= jobHistoryLimit {
		return "", errors.New("session job history limit reached")
	}
	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(idBytes[:])
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	j := &job{exit: -1, cancel: cancel}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Stdout = j
	cmd.Stderr = j
	cmd.Env = jobEnv(s.Config.Home.Home, trial)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Give Pix time to release its sandbox before killing stubborn descendants.
	finished := make(chan struct{})
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
		go func() {
			timer := time.NewTimer(3 * time.Second)
			defer timer.Stop()
			select {
			case <-finished:
				return
			case <-timer.C:
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		}()
		return err
	}
	cmd.WaitDelay = 4 * time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		return "", errors.New("could not start host command: " + s.redact(err.Error()))
	}
	if s.jobs == nil {
		s.jobs = map[string]*job{}
	}
	s.jobs[id] = j
	s.order = append(s.order, id)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-finished:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					if s.Config.Guard == nil || s.Config.Guard() != nil {
						cancel()
						return
					}
				}
			}
		}()
		err := cmd.Wait()
		close(finished)
		// A successful parent can leave background descendants holding host authority.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		j.mu.Lock()
		j.done = true
		j.exit = 0
		if err != nil {
			j.exit = cmd.ProcessState.ExitCode()
		}
		j.mu.Unlock()
	}()
	return encode(map[string]any{"job_id": id, "status": "running"}), nil
}
func (s *Service) status(id string, cancel bool) (string, error) {
	s.mu.Lock()
	j := s.jobs[id]
	s.mu.Unlock()
	if j == nil {
		return "", errors.New("unknown job in this session")
	}
	if cancel {
		j.cancel()
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	status := "running"
	if j.done {
		status = "finished"
	}
	return encode(map[string]any{"job_id": id, "status": status, "exit_code": j.exit, "output": s.redact(string(j.output)), "truncated": j.truncated}), nil
}
func (s *Service) redact(v string) string {
	if s.Config.Redact != nil {
		return s.Config.Redact(v)
	}
	return v
}
func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	for _, j := range s.jobs {
		j.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}
func jobEnv(home string, trial bool) []string {
	out := []string{}
	for _, key := range strings.Fields("PATH HOME USER LOGNAME SHELL TMPDIR LANG LC_ALL SSH_AUTH_SOCK DOCKER_HOST DOCKER_CONTEXT DOCKER_CONFIG OLLAMA_HOST OP_BIOMETRIC_UNLOCK_ENABLED") {
		if value, ok := os.LookupEnv(key); ok {
			out = append(out, key+"="+value)
		}
	}
	out = append(out, "PIX_HOME="+home)
	if trial {
		out = append(out, "PIX_HOST_TOOLS_DISABLED=1")
	}
	return out
}
