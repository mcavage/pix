package health

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// daemon.go — the probe for a pack's supervised host daemons.
//
// It exists because these were the one host-exec facet with no row in the
// report at all. `snow-proxy` is the motivating case: it is how the sandbox
// reaches Snowflake, and when it died the `warehouse` capability degraded in
// total silence — `pix doctor` had nothing to say because a `[[proxy]]` and a
// LaunchAgent are invisible to it. A capability that can fail without the
// report noticing is the same defect the MCP probe was rebuilt to remove, just
// one facet over.
//
// It asks the daemon the SAME question the supervisor asks: dial the loopback
// port, or GET the declared health path. Not "is a process running" — a wedged
// daemon holds its pid and answers nothing, and that is the case a LaunchAgent's
// KeepAlive cannot see either.

// DaemonServer is one supervised daemon, already resolved by the caller from the
// active pack's [[services]] entries.
type DaemonServer struct {
	Name string
	// Listen defaults to 127.0.0.1; Port is the loopback port it binds.
	Listen string
	Port   int
	// Health is "tcp" (a successful dial is the answer) or an absolute HTTP path.
	Health string
	// Fix is the exact command that repairs this daemon, or empty when the
	// caller could not establish one — in which case the probe reports the gap
	// without inventing a repair.
	Fix string
	// Unpinned marks a daemon identified by a PATH-resolved command rather than
	// a SHA-pinned executable. Surfaced as evidence, never as a gap: it is a
	// property the user consented to on the adoption screen, not a fault.
	Unpinned bool
}

func (d DaemonServer) addr() string {
	host := d.Listen
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(d.Port))
}

// DaemonProbe checks every supervised daemon the active pack declares.
type DaemonProbe struct {
	// Servers is the declared set, in manifest order. Empty means this host runs
	// no pack daemons, which is a perfectly healthy state.
	Servers []DaemonServer
	// Dial and Get are the seams a test drives. Nil means the real network.
	Dial func(network, addr string, budget time.Duration) error
	Get  func(url string, budget time.Duration) (int, error)
}

func (DaemonProbe) Name() string { return "daemons" }

// Required is false, matching every other pack-contributed facet: a pack's
// daemon is opt-in, so a broken one must be VISIBLE without failing the exit
// code of every script that runs `pix doctor`.
func (DaemonProbe) Required() bool { return false }

func (p DaemonProbe) Check(ctx context.Context) Result {
	if len(p.Servers) == 0 {
		return Result{Name: p.Name(), Status: StatusReady, Detail: "none declared",
			Evidence: "the active pack declares no supervised daemons"}
	}
	findings := make([]mcpFinding, 0, len(p.Servers))
	for _, d := range p.Servers {
		findings = append(findings, p.checkOne(ctx, d))
	}
	// One shared reducer with the MCP probe: the precedence is identical (a
	// verified gap dominates, then anything unproven, then ready), and two copies
	// would be two places for "what does this report claim" to drift.
	return reduceFindings(p.Name(), findings, len(p.Servers), "answering")
}

func (p DaemonProbe) checkOne(ctx context.Context, d DaemonServer) mcpFinding {
	unpinned := ""
	if d.Unpinned {
		unpinned = " (unpinned: found by name at launch)"
	}
	err := p.ask(ctx, d)
	if err == nil {
		return mcpFinding{name: d.Name, note: d.Name + ": answering on " + d.addr() + unpinned}
	}
	// A daemon that does not answer is a VERIFIED gap, not an unknown: we asked
	// the exact question the sandbox asks and got a definite no.
	return mcpFinding{name: d.Name, gap: true, fix: d.Fix,
		note: fmt.Sprintf("%s: not answering on %s (%v)%s", d.Name, d.addr(), err, unpinned)}
}

func (p DaemonProbe) ask(ctx context.Context, d DaemonServer) error {
	budget := DefaultBudget
	if dl, ok := ctx.Deadline(); ok {
		if remaining := time.Until(dl); remaining > 0 && remaining < budget {
			budget = remaining
		}
	}
	if d.Health == "tcp" || !strings.HasPrefix(d.Health, "/") {
		if p.Dial != nil {
			return p.Dial("tcp", d.addr(), budget)
		}
		conn, err := net.DialTimeout("tcp", d.addr(), budget)
		if err != nil {
			return err
		}
		return conn.Close()
	}
	url := "http://" + d.addr() + d.Health
	if p.Get != nil {
		code, err := p.Get(url, budget)
		if err != nil {
			return err
		}
		return httpHealthy(code, d.Health)
	}
	client := &http.Client{
		Timeout:       budget,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return httpHealthy(resp.StatusCode, d.Health)
}

func httpHealthy(code int, path string) error {
	if code < 200 || code > 299 {
		return fmt.Errorf("%s returned HTTP %d", path, code)
	}
	return nil
}
