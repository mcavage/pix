// fixture is a REAL go-plugin binary used by the supervise tests. It is not a
// mock: the tests exec this program, speak the actual go-plugin handshake to
// it, and make real net/rpc calls. Its behaviour is driven by env vars so one
// binary can play every failure mode the supervisor must survive.
//
//	FIXTURE_CRASH_MS=<n>   exit(3) n ms after the handshake (crash/backoff)
//	FIXTURE_UNHEALTHY=1    Check() fails (health-budget eviction)
//	FIXTURE_STUBBORN=1     ignore SIGTERM/SIGINT (stop-budget escalation)
//	FIXTURE_ENV_DUMP=<f>   write the received environment to f (env allowlist)
//	FIXTURE_SPAWN_LOG=<f>  append one line per process start (restart counting)
//	FIXTURE_TAG=<s>        reported by Describe(), so a test can tell two
//	                       generations of the same unit apart
package main

import (
	"errors"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	goplugin "github.com/hashicorp/go-plugin"

	"pix/host/plugin"
)

type fixture struct{ tag string }

func (f fixture) Mint(audience string, scopes []string) (plugin.Token, error) {
	return plugin.Token{AccessToken: "fixture-" + audience, TokenType: "Bearer", ExpiresIn: 60}, nil
}

func (f fixture) Check() error {
	if os.Getenv("FIXTURE_UNHEALTHY") == "1" {
		return errors.New("fixture is unhealthy on purpose")
	}
	return nil
}

func (f fixture) Describe() (plugin.BrokerInfo, error) {
	return plugin.BrokerInfo{Name: "fixture", AuthHeader: f.tag, DefaultPort: os.Getpid()}, nil
}

func main() {
	if dump := os.Getenv("FIXTURE_ENV_DUMP"); dump != "" {
		var buf []byte
		for _, kv := range os.Environ() {
			buf = append(buf, kv...)
			buf = append(buf, '\n')
		}
		_ = os.WriteFile(dump, buf, 0o600)
	}
	if log := os.Getenv("FIXTURE_SPAWN_LOG"); log != "" {
		if f, err := os.OpenFile(log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			_, _ = f.WriteString(strconv.Itoa(os.Getpid()) + "\n")
			_ = f.Close()
		}
	}
	if os.Getenv("FIXTURE_STUBBORN") == "1" {
		signal.Notify(make(chan os.Signal, 4), syscall.SIGTERM, syscall.SIGINT)
	}
	if ms, err := strconv.Atoi(os.Getenv("FIXTURE_CRASH_MS")); err == nil && ms >= 0 {
		go func() {
			time.Sleep(time.Duration(ms) * time.Millisecond)
			os.Exit(3)
		}()
	}
	impl := fixture{tag: os.Getenv("FIXTURE_TAG")}
	plugin.Serve(map[string]goplugin.Plugin{"broker": &plugin.BrokerPlugin{Impl: impl}})
}
