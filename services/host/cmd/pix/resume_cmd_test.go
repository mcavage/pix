package main

import (
	"reflect"
	"testing"
)

func TestResumeOptsTargetsExactSession(t *testing.T) {
	dir := t.TempDir()
	cmd := resumeCmd{
		Session: "019fd77b-0295-79d5-8411-ef30ac524994",
		Dir:     dir,
	}

	got, err := cmd.opts()
	if err != nil {
		t.Fatalf("resume opts: %v", err)
	}
	if got.Workspace != dir {
		t.Errorf("workspace = %q, want %q", got.Workspace, dir)
	}
	want := []string{"--session", cmd.Session}
	if !reflect.DeepEqual(got.Passthrough, want) {
		t.Errorf("passthrough = %q, want %q", got.Passthrough, want)
	}
}

func TestResumeOptsRejectsMissingWorkspace(t *testing.T) {
	cmd := resumeCmd{
		Session: "019fd77b-0295-79d5-8411-ef30ac524994",
		Dir:     "/definitely/not/a/pix/workspace",
	}
	if _, err := cmd.opts(); err == nil {
		t.Fatal("resume accepted a missing workspace")
	}
}
