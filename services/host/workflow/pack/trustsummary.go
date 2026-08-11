package pack

import (
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"

	"pix/host/packinfo"
)

// trustsummary.go — the DEFAULT pack consent screen.
//
// The detailed screen (renderHostBoMDetails) is complete and correct, and for a
// real pack it is about seventy lines. Mark read one as a new user and said: "my
// god this is an overwhelming wall of text". He is right, and the consequence is
// not cosmetic — a consent screen nobody reads is not consent. Length was
// actively costing the thing the gate exists for.
//
// Most of that length was REPETITION rather than information. `slack` appeared
// once as a Host MCP with its health check, and again as an `Ensures:` block with
// the same health check and its own guidance paragraph. Per-integration grouping
// also scattered the one question a user actually has — what will execute on my
// machine — across the whole screen, so answering it meant reading all of it.
//
// So the summary inverts the grouping: counts first, then ONE flat list of every
// command that runs on this Mac. That is a better answer to the security question
// than the detailed view gives, not a weaker one.
//
// The rule this file must not break: everything the FINGERPRINT covers has to be
// visible here. If a fact re-gates an already-accepted pack, the user must be
// able to see what changed on the screen they are being re-asked on — otherwise
// re-consent is a mystery prompt. Pack-authored HINTS are the one thing moved
// behind `--details`, and they are prose that cannot change what executes.

// hostRunLine is one command that will execute on this Mac, with a note for the
// ones that take over the terminal or open a browser.
type hostRunLine struct {
	cmd  string
	note string
}

// hostRunCommands is every command the pack causes to run on the host, in the
// order a user meets them: servers and daemons first, then the setup steps that
// install and authorize them.
//
// Health probes are NOT here. They also execute on the host, so they are counted
// and offered separately rather than folded in — a list captioned "runs on this
// Mac" that silently omitted four executable argv would be a false claim, and
// one captioned that way while including them would misdescribe when they run.
func hostRunCommands(b hostBoM) []hostRunLine {
	var out []hostRunLine
	for _, m := range b.MCP {
		cmd := strings.Join(m.Argv, " ")
		if len(m.EnvKeys) > 0 {
			// The op-run wrapper is what actually runs, so it is what is shown.
			cmd = "op run -- " + cmd
		}
		out = append(out, hostRunLine{cmd: cmd})
	}
	for _, c := range b.Containers {
		image := c.Image
		if image == "" {
			image = "manifest " + c.Manifest
		}
		out = append(out, hostRunLine{cmd: "docker run " + image})
	}
	for _, svc := range b.Services {
		switch {
		case svc.Runtime == packinfo.ServiceRuntimeContainer:
			out = append(out, hostRunLine{cmd: "docker run " + svc.Image})
		case svc.Command != "":
			out = append(out, hostRunLine{cmd: joinArgv(svc.Command, svc.Argv), note: "unpinned"})
		default:
			out = append(out, hostRunLine{cmd: joinArgv(svc.Path, svc.Argv)})
		}
	}
	for _, s := range b.Setup {
		if !s.Declarative() {
			// An executable hook runs pack-supplied code, and BOTH argv execute:
			// check every time, apply when check fails.
			out = append(out, hostRunLine{cmd: joinArgv(s.Path, s.ApplyArgs)})
			continue
		}
		for _, a := range s.Apply {
			note := ""
			if a.Kind == "interactive" {
				note = "opens a browser"
			}
			out = append(out, hostRunLine{cmd: strings.Join(a.Argv, " "), note: note})
		}
	}
	return out
}

// hostProbeCommands is every argv `pix doctor` and `pix setup` execute to CHECK
// something. They run on this Mac too, which is why they are disclosed rather
// than assumed uninteresting.
func hostProbeCommands(b hostBoM) []string {
	var out []string
	add := func(argv []string) {
		if len(argv) > 0 {
			out = append(out, strings.Join(argv, " "))
		}
	}
	for _, m := range b.MCP {
		add(m.Probe)
	}
	for _, c := range b.Containers {
		add(c.Probe)
	}
	for _, r := range b.RemoteMCP {
		add(r.Probe)
	}
	for _, s := range b.Setup {
		if !s.Declarative() {
			add(append([]string{s.Path}, s.CheckArgs...))
			continue
		}
		for _, r := range s.Require {
			if r.Kind == "probe" {
				add(r.Argv)
			}
		}
	}
	return out
}

// handedToServers is every env name a host MCP server receives that is NOT
// already listed as a solicited credential, plus literal values shown as KEY=VALUE
// (a literal is not a secret, and hiding it would hide a pack changing what a
// server is configured with).
func handedToServers(b hostBoM) []string {
	isCred := map[string]bool{}
	for _, c := range b.Creds {
		isCred[c] = true
	}
	seen := map[string]bool{}
	var out []string
	addKey := func(k string) {
		if k == "" || isCred[k] || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, k)
	}
	for _, m := range b.MCP {
		for _, k := range m.EnvKeys {
			addKey(k)
		}
	}
	for _, c := range b.Containers {
		for _, k := range c.EnvKeys {
			addKey(k)
		}
		values := make([]string, 0, len(c.EnvValues))
		for k, v := range c.EnvValues {
			values = append(values, k+"="+v)
		}
		sort.Strings(values)
		for _, kv := range values {
			if !seen[kv] {
				seen[kv] = true
				out = append(out, kv)
			}
		}
	}
	for _, svc := range b.Services {
		for _, k := range svc.Env {
			addKey(k)
		}
	}
	return out
}

func joinArgv(head string, argv []string) string {
	if len(argv) == 0 {
		return head
	}
	return head + " " + strings.Join(argv, " ")
}

// plural is spelled out because "1 MCP servers" undermines a screen whose whole
// job is to look like it was written on purpose.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// gatewayHosts reduces the model gateways to the distinct hosts they reach. Nine
// gateway lines on the old screen were three hosts repeated, and the fact a user
// needs is where their prompts go.
func gatewayHosts(b hostBoM) []string {
	seen := map[string]bool{}
	var hosts []string
	for _, inf := range b.Inference {
		h := inf.URL
		if u, err := url.Parse(inf.URL); err == nil && u.Host != "" {
			h = u.Host
		}
		if !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	sort.Strings(hosts)
	return hosts
}

// authModes reduces the gateways' credential-routing policy to its distinct
// values, so "sbx-session" is stated once instead of per gateway.
func authModes(b hostBoM) []string {
	seen := map[string]bool{}
	var modes []string
	for _, inf := range b.Inference {
		a := inf.Auth
		if inf.Service != "" {
			a += " via " + inf.Service
		}
		if !seen[a] {
			seen[a] = true
			modes = append(modes, a)
		}
	}
	sort.Strings(modes)
	return modes
}

// row prints one summary line: a count-and-label column, then the detail.
func row(out io.Writer, label, detail string) {
	fmt.Fprintf(out, "  %-18s %s\n", label, detail)
}

// contRow continues the previous row's detail column with no new label.
func contRow(out io.Writer, detail string) {
	fmt.Fprintf(out, "  %-18s %s\n", "", detail)
}

// renderHostBoMSummary prints the default consent screen.
func renderHostBoMSummary(out io.Writer, b hostBoM) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "This pack adds to Pix:")
	fmt.Fprintln(out)

	if hosted := len(b.MCP) + len(b.Containers); hosted+len(b.RemoteMCP) > 0 {
		total := hosted + len(b.RemoteMCP)
		parts := make([]string, 0, 2)
		if hosted > 0 {
			parts = append(parts, fmt.Sprintf("%d on this Mac", hosted))
		}
		if len(b.RemoteMCP) > 0 {
			names := make([]string, 0, len(b.RemoteMCP))
			for _, r := range b.RemoteMCP {
				names = append(names, r.Name)
			}
			// Remote servers are NAMED, not just counted: a remote MCP is where
			// conversation content goes, and "4 remote" does not let a user object
			// to a specific destination. Full URLs are in --details.
			parts = append(parts, fmt.Sprintf("%d remote (%s)", len(b.RemoteMCP), strings.Join(names, ", ")))
		}
		row(out, fmt.Sprintf("%d MCP %s", total, plural(total, "server", "servers")), strings.Join(parts, ", "))
		// Remote DESTINATIONS, at host granularity. A remote MCP is where
		// conversation content goes, and the URL is fingerprinted — so a pack that
		// repointed one would re-gate, and a user re-asked on a screen showing only
		// the name could not see what changed.
		//
		// FULL URLs, one per line. Host-only was tempting and wrong: the endpoint
		// is what the fingerprint covers, so host-only left a repointed path
		// re-gating a user who could not see any change. Four extra lines is a
		// cheap price for the gate meaning what it says.
		for _, r := range b.RemoteMCP {
			contRow(out, fmt.Sprintf("%s → %s", r.Name, r.URL))
		}
	}

	if n := len(b.Inference); n > 0 {
		detail := "all via " + strings.Join(gatewayHosts(b), ", ")
		if modes := authModes(b); len(modes) > 0 {
			detail += " (" + strings.Join(modes, "; ") + ")"
		}
		row(out, fmt.Sprintf("%d model %s", n, plural(n, "gateway", "gateways")), detail)
	}

	for _, svc := range b.Services {
		where := ""
		if svc.Port != 0 {
			listen := svc.Listen
			if listen == "" {
				listen = "127.0.0.1"
			}
			where = fmt.Sprintf(" %s:%d", listen, svc.Port)
		}
		detail := svc.Name + where
		if svc.Runtime == packinfo.ServiceRuntimeDaemon && svc.Command != "" {
			// UNPINNED stays on the DEFAULT screen. It is the one host-exec facet
			// with a weaker guarantee than its neighbours, and burying it behind a
			// flag would be choosing not to be asked about it.
			detail += "  UNPINNED"
		}
		row(out, "1 host service", detail)
	}

	for _, pr := range b.SandboxProxies {
		dest := "a declared endpoint"
		if len(pr.Egress) > 0 {
			dest = strings.Join(pr.Egress, ", ")
		}
		row(out, "1 sandbox command", fmt.Sprintf("%s → %s", pr.Name, dest))
	}
	for _, pr := range b.Proxies {
		row(out, "1 host wrapper", fmt.Sprintf("%s (bin/%s; `pix host` only)", pr, pr))
	}
	for _, bn := range b.Bins {
		row(out, "1 external binary", fmt.Sprintf("%s  sha256:%s", bn.Name, strings.ToLower(strings.TrimSpace(bn.SHA))))
	}

	if n := len(b.Creds); n > 0 {
		row(out, fmt.Sprintf("%d %s", n, plural(n, "credential", "credentials")), strings.Join(b.Creds, ", "))
		contRow(out, "(1Password references; pix never stores the values)")
	}
	// Everything ELSE a host server is handed. b.Creds is only each integration's
	// primary credential, while EnvKeys and literal EnvValues are separately
	// fingerprinted — so without this a pack could add an env name to a host
	// command, re-gate every user, and show them nothing new.
	if extra := handedToServers(b); len(extra) > 0 {
		row(out, "also handed", strings.Join(extra, ", "))
	}

	if n := len(b.Setup); n > 0 {
		ids := make([]string, 0, n)
		for _, s := range b.Setup {
			ids = append(ids, s.ID)
		}
		row(out, fmt.Sprintf("%d setup %s", n, plural(n, "step", "steps")), strings.Join(ids, ", "))
	}

	coveredEgress := map[string]bool{}
	for _, proxy := range b.SandboxProxies {
		for _, endpoint := range proxy.Egress {
			coveredEgress[endpoint] = true
		}
	}
	var extraEgress []string
	for _, endpoint := range b.Egress {
		if !coveredEgress[endpoint] {
			extraEgress = append(extraEgress, endpoint)
		}
	}
	if len(extraEgress) > 0 {
		row(out, "network access", strings.Join(extraEgress, ", "))
	}

	// The list. This is the screen's real payload: one place answering "what will
	// execute on my machine", which the per-integration view never gathered.
	if runs := hostRunCommands(b); len(runs) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "  Runs on this Mac:")
		width := 0
		for _, r := range runs {
			if r.note != "" && len(r.cmd) > width {
				width = len(r.cmd)
			}
		}
		for _, r := range runs {
			if r.note == "" {
				fmt.Fprintf(out, "    %s\n", r.cmd)
				continue
			}
			fmt.Fprintf(out, "    %-*s  [%s]\n", width, r.cmd, r.note)
		}
	}

	// Probes are disclosed as what they are: commands that also run here. Counted
	// rather than listed, because they are checks rather than actions — but said
	// out loud, because a count of things that execute is not the same as silence.
	if probes := hostProbeCommands(b); len(probes) > 0 {
		// A count with no way to see the list is a dead end. `d` at the prompt and
		// --details both expand it.
		fmt.Fprintf(out, "\n  Health checks, also run here: %d %s (d to list)\n",
			len(probes), plural(len(probes), "command", "commands"))
	}

	if len(b.Prerequisites) > 0 {
		fmt.Fprintln(out, "\nBefore continuing, make sure:")
		for _, item := range b.Prerequisites {
			fmt.Fprintf(out, "  • %s\n", item)
		}
	}
}

// renderHostBoM prints the consent screen: the summary by default, every field
// when the user asked for detail.
func renderHostBoM(out io.Writer, b hostBoM, details bool) {
	if details {
		renderHostBoMDetails(out, b)
		return
	}
	renderHostBoMSummary(out, b)
}
