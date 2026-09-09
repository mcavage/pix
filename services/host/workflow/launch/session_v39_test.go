//go:build unix

package launch

import (
	"pix/host/lease"
	"pix/host/sandbox"
	"testing"
)

func TestPromoteSessionCreationV39SharedListing(t *testing.T) {
	isolateState(t)
	installFakeSbx(t, `
if [ "$1" = "ls" ] && [ "$2" = "--json" ]; then
 echo '{"sandboxes":[{"name":"unrelated","id":"943fa6d6-7c2e-4d8e-9886-0b2dcec1ca62","agent":"shell","status":"running","ports":[{"host_ip":"127.0.0.1","host_port":36631,"sandbox_port":4173,"protocol":"tcp4"}]},{"name":"pix-demo","id":"5c2b6e0a-1f3d-4a9b-8e21-7d4f2b6c9a10","agent":"pix","status":"running","workspaces":["/workspace"]}]}'
 exit 0
fi
exit 1
`)
	key := SessionName(t.TempDir())
	dir, err := leaseDirFor(key)
	if err != nil {
		t.Fatal(err)
	}
	fp := sandbox.Fingerprint{"env.A": "1"}
	if err := WriteCreateIntent(dir, CreateIntent{EnvironmentRoot: "/envs/work", EnvironmentName: "work", SandboxName: "pix-demo", Fingerprint: fp}); err != nil {
		t.Fatal(err)
	}
	recorded, warning, err := PromoteSessionCreation(realEnv(), key, "pix-demo", fp, []string{"--session", "conversation"})
	if !recorded || warning != nil || err != nil {
		t.Fatalf("promotion = %v, %v, %v", recorded, warning, err)
	}
	rec, err := lease.ReadRecord(dir)
	if err != nil || rec.InstanceID != "5c2b6e0a-1f3d-4a9b-8e21-7d4f2b6c9a10" || rec.Name != "pix-demo" {
		t.Fatalf("receipt = %+v, %v", rec, err)
	}
}
