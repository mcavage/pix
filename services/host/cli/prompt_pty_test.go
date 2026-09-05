package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

type promptPTYCommand struct{}

func (*promptPTYCommand) Run(d *Deps) error {
	if !d.AskYN("Continue? [y/N] ", false) {
		return fmt.Errorf("confirmation declined")
	}
	for _, label := range []string{"Google keyring in 1Password", "Google Workspace email"} {
		answer, ok := d.Ask(Question{Label: label})
		if !ok {
			return fmt.Errorf("answer missing")
		}
		fmt.Printf("ACCEPTED:%s\n", answer)
	}
	return nil
}

func TestPromptPTYChild(t *testing.T) {
	if os.Getenv("PIX_PROMPT_PTY_CHILD") != "1" {
		return
	}
	d := &Deps{In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Interactive: true}
	err := RunRoot[promptPTYCommand]("prompt", "", "", nil, d)
	fmt.Printf("EXIT:%d\n", ExitCode(err))
}

func TestPromptPTYEditing(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required for the real terminal fixture")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := `
import os, pty, fcntl, termios, struct, subprocess, select, time, sys
master, slave = pty.openpty()
original = termios.tcgetattr(slave)
fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 40, 132, 0, 0))
p = subprocess.Popen([sys.argv[1], '-test.run=^TestPromptPTYChild$'], stdin=slave, stdout=slave, stderr=slave, env=dict(os.environ, PIX_PROMPT_PTY_CHILD='1'))
def until(marker):
 data=b''; deadline=time.monotonic()+10
 while marker not in data:
  assert time.monotonic()<deadline, ('prompt stalled',data)
  if select.select([master],[],[],0.1)[0]: data+=os.read(master,65536)
 return data
try:
 until(b'Continue? [y/N] '); os.write(master,b'y\r')
 until(b'Google keyring in 1Password: ')
 reference=b'op://pix-gm-pack/GOG_KEYRING_PASSWORD/credential'
 os.write(master,reference+b'xxxxxxxxxx')
 echoed=until(b'xxxxxxxxxx')
 assert b'\r\n' not in echoed, ('line wrapped before the real terminal edge',echoed)
 os.write(master,b'\x7f'*10+b'\r')
 accepted=until(b'Google Workspace email: ')
 assert b'ACCEPTED:'+reference+b'\r\n' in accepted, accepted
 os.write(master,b'mark.cavage@gmail'+b'\x08'*5+b'docker.coX\x7fm\x1b[D\x1b[C\r')
 accepted=until(b'ACCEPTED:mark.cavage@docker.com\r\n')
 p.wait(timeout=5)
 assert p.returncode==0,accepted
 restored=termios.tcgetattr(slave)
 restored[3] &= ~getattr(termios,'PENDIN',0)
 original[3] &= ~getattr(termios,'PENDIN',0)
 assert restored==original, ('terminal mode was not restored',original,restored)
finally:
 if p.poll() is None: p.kill(); p.wait()
 os.close(master)
 os.close(slave)
`
	for _, mode := range []string{"edit", "cancel", "paste"} {
		testScript := script
		if mode == "cancel" {
			start := strings.Index(testScript, " reference=b")
			end := strings.Index(testScript, " p.wait(timeout=5)")
			testScript = testScript[:start] + " os.write(master,b'partial\\x03')\n accepted=until(b'EXIT:130\\r\\n')\n assert b'Google Workspace email:' not in accepted, accepted\n" + testScript[end:]
		}
		if mode == "paste" {
			testScript = strings.Replace(testScript, "os.write(master,reference+b'xxxxxxxxxx')", "os.write(master,b'\\x1b[200~'+reference+b'\\x1b[201~'+b'xxxxxxxxxx')", 1)
		}
		cmd := exec.Command(python, "-c", testScript, binary)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("terminal editing (%s): %v\n%s", mode, err, output)
		}
	}
}
