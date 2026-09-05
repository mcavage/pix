package sandbox

import (
	"reflect"
	"testing"
)

func TestCreateArgv_TTYUsesCombinedFlag(t *testing.T) {
	argv, err := CreateArgv(CreateOpts{Name: "pix-demo", Image: "docker.io/mcavage/pix:0.1.0", TTY: true})
	if err != nil {
		t.Fatalf("CreateArgv: %v", err)
	}
	want := []string{"create", "--name", "pix-demo", "-it", "docker.io/mcavage/pix:0.1.0"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("CreateArgv = %v, want %v", argv, want)
	}
}

func TestCreateArgv_NonTTYUsesDashI(t *testing.T) {
	argv, err := CreateArgv(CreateOpts{Name: "pix-demo", Image: "img", TTY: false, Command: []string{"pi", "--version"}})
	if err != nil {
		t.Fatalf("CreateArgv: %v", err)
	}
	want := []string{"create", "--name", "pix-demo", "-i", "img", "pi", "--version"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("CreateArgv = %v, want %v", argv, want)
	}
}

func TestCreateArgv_RequiresNameAndImage(t *testing.T) {
	if _, err := CreateArgv(CreateOpts{Image: "img"}); err == nil {
		t.Fatalf("CreateArgv with no name = nil error, want one")
	}
	if _, err := CreateArgv(CreateOpts{Name: "pix-demo"}); err == nil {
		t.Fatalf("CreateArgv with no image = nil error, want one")
	}
}

func TestExecArgv_TTYAndNonTTY(t *testing.T) {
	argv, err := ExecArgv(ExecOpts{Name: "pix-demo", TTY: true, Command: []string{"pi"}})
	if err != nil {
		t.Fatalf("ExecArgv: %v", err)
	}
	if want := []string{"exec", "-it", "pix-demo", "--", "pi"}; !reflect.DeepEqual(argv, want) {
		t.Fatalf("ExecArgv(tty) = %v, want %v", argv, want)
	}

	argv, err = ExecArgv(ExecOpts{Name: "pix-demo", TTY: false, Command: []string{"pi", "-p", "hi"}})
	if err != nil {
		t.Fatalf("ExecArgv: %v", err)
	}
	if want := []string{"exec", "-i", "pix-demo", "--", "pi", "-p", "hi"}; !reflect.DeepEqual(argv, want) {
		t.Fatalf("ExecArgv(non-tty) = %v, want %v", argv, want)
	}
}

func TestExecArgv_RequiresNameAndCommand(t *testing.T) {
	if _, err := ExecArgv(ExecOpts{Command: []string{"pi"}}); err == nil {
		t.Fatalf("ExecArgv with no name = nil error, want one")
	}
	if _, err := ExecArgv(ExecOpts{Name: "pix-demo"}); err == nil {
		t.Fatalf("ExecArgv with no command = nil error, want one")
	}
}

var (
	create = CreateOpts{Name: "pix-demo", Image: "img"}
	exec   = ExecOpts{Name: "pix-demo", Command: []string{"pi"}}
)

func TestPlanLaunch_AbsentCreates(t *testing.T) {
	dec, argv, err := PlanLaunch(nil, create, exec)
	if err != nil {
		t.Fatalf("PlanLaunch: %v", err)
	}
	if dec != DecisionCreate {
		t.Fatalf("Decision = %v, want DecisionCreate", dec)
	}
	if len(argv) == 0 || argv[0] != "create" {
		t.Fatalf("argv = %v, want a create argv", argv)
	}
}

func TestPlanLaunch_RunningVerifiedExecs(t *testing.T) {
	found := &Entry{Name: "pix-demo", State: StateRunning, IdentityVerified: true}
	dec, argv, err := PlanLaunch(found, create, exec)
	if err != nil {
		t.Fatalf("PlanLaunch: %v", err)
	}
	if dec != DecisionExec {
		t.Fatalf("Decision = %v, want DecisionExec", dec)
	}
	if len(argv) == 0 || argv[0] != "exec" {
		t.Fatalf("argv = %v, want an exec argv", argv)
	}
}

func TestPlanLaunch_UnverifiedRefuses(t *testing.T) {
	found := &Entry{Name: "pix-demo", State: StateRunning, IdentityVerified: false}
	dec, argv, err := PlanLaunch(found, create, exec)
	if err == nil {
		t.Fatalf("PlanLaunch(unverified) = nil error, want one")
	}
	if dec != DecisionNone || argv != nil {
		t.Fatalf("PlanLaunch(unverified) = (%v, %v), want (DecisionNone, nil) alongside the error", dec, argv)
	}
}

func TestPlanLaunch_StoppedRefuses(t *testing.T) {
	found := &Entry{Name: "pix-demo", State: StateStopped, IdentityVerified: true}
	if _, _, err := PlanLaunch(found, create, exec); err == nil {
		t.Fatalf("PlanLaunch(stopped) = nil error, want one (exec cannot attach to a stopped sandbox)")
	}
}

func TestPlanLaunch_UnknownStateRefuses(t *testing.T) {
	found := &Entry{Name: "pix-demo", State: StateUnknown, IdentityVerified: true}
	if _, _, err := PlanLaunch(found, create, exec); err == nil {
		t.Fatalf("PlanLaunch(unknown state) = nil error, want one")
	}
}
