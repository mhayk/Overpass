package integration_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// The services run as real processes, driven over HTTP and NATS.
//
// Not by importing their packages: Go forbids reaching into another module's
// internal/, and that restriction happens to be pointing at the right design.
// An integration test that calls an unexported constructor is testing an API
// no deployment uses. These tests can only touch what a client can touch, so
// they cannot accidentally depend on a seam that does not exist in production —
// and killing a process mid-transaction, which is one of the scenarios, is not
// something an in-process test can do at all.

type service struct {
	name    string
	baseURL string
	cmd     *exec.Cmd
	logs    *syncBuffer
	mu      sync.Mutex
	stopped bool
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// build compiles a service once, into a temp dir shared by the whole package.
func build(root, module, binName string) (string, error) {
	out := filepath.Join(binDir, binName)
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	//nolint:noctx,gosec // a build has no caller to cancel it and the args are fixed
	cmd := exec.Command("go", "build", "-o", out, "./cmd/"+binName)
	cmd.Dir = filepath.Join(root, "services", module)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("building %s: %w\n%s", binName, err, output)
	}
	return out, nil
}

// start runs a built binary and waits for it to report ready.
func start(binary, name string, extraEnv map[string]string) (*service, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	// Deliberately NOT CommandContext. These processes are killed explicitly, and
	// often with SIGKILL mid-transaction — that is the scenario. Tying them to a
	// context would let the test framework reap them politely instead.
	//nolint:noctx,gosec // see above; the binary is one this package built
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(),
		"DATABASE_URL="+env.dsn,
		"NATS_URL="+env.natsURL,
		"LOG_LEVEL=debug",
		"SHUTDOWN_TIMEOUT=5",
	)
	for k, v := range extraEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Env = append(cmd.Env, addrVar(name)+"="+addr)

	logs := &syncBuffer{}
	cmd.Stdout = logs
	cmd.Stderr = logs

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", name, err)
	}

	svc := &service{name: name, baseURL: "http://" + addr, cmd: cmd, logs: logs}
	if err := svc.waitReady(45 * time.Second); err != nil {
		_ = svc.Kill() //nolint:errcheck // already failing
		return nil, fmt.Errorf("%s never became ready: %w\n%s", name, err, logs.String())
	}
	return svc, nil
}

func addrVar(name string) string {
	switch name {
	case "tasking-api":
		return "TASKING_API_ADDR"
	case "plan-gateway":
		return "PLAN_GATEWAY_ADDR"
	case "planner":
		return "PLANNER_ADDR"
	default:
		return "HTTP_ADDR"
	}
}

// waitReady polls /readyz rather than sleeping. A fixed sleep is either too
// short on a loaded CI runner or wasted time on a laptop, and it is the single
// most common source of a flaky integration suite.
func (s *service) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	var last error

	for time.Now().Before(deadline) {
		if s.cmd.ProcessState != nil && s.cmd.ProcessState.Exited() {
			return fmt.Errorf("process exited with %s", s.cmd.ProcessState)
		}
		req, err := http.NewRequestWithContext(context.Background(),
			http.MethodGet, s.baseURL+"/readyz", nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			last = err
			time.Sleep(200 * time.Millisecond)
			continue
		}
		_ = resp.Body.Close() //nolint:errcheck // status is all we need
		if resp.StatusCode == http.StatusOK {
			return nil
		}
		last = fmt.Errorf("readyz returned %d", resp.StatusCode)
		time.Sleep(200 * time.Millisecond)
	}
	return last
}

// Kill stops the process abruptly, with no chance to finish what it was doing.
//
// SIGKILL, not SIGTERM, and that is the point of having it: a graceful
// shutdown proves nothing about crash safety. The scenarios that matter are the
// ones where the process does not get to run its deferred functions.
func (s *service) Kill() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.cmd.Process == nil {
		return nil
	}
	s.stopped = true
	if err := s.cmd.Process.Kill(); err != nil {
		return err
	}
	_ = s.cmd.Wait() //nolint:errcheck // killed on purpose; the status is expected to be non-zero
	return nil
}

func freePort() (int, error) {
	//nolint:noctx // a port probe that completes before it could be cancelled
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close() //nolint:errcheck // just releasing the probe
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("unexpected address type %T", listener.Addr())
	}
	return addr.Port, nil
}
