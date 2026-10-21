#!/usr/bin/env python3
"""Build and exercise upstream Pi with isolated fake-auth home; no API/MCP calls."""
import json
import os
import fcntl
from pathlib import Path
import pty
import queue
import re
import select
import struct
import subprocess
import tempfile
import termios
import threading
import time
import uuid

ROOT=Path(__file__).resolve().parents[2]
BASE='docker.io/mcavage/marsh-pi@sha256:570459f4ec8c65427628d36bfaa870afefb6d058cbcf2e5f2f5df4697960e95a'

def rpc(image, home, commands, mounts=()):
    # BuildKit's private cache is not the Docker Engine image store. A cold
    # engine may need this pull; keep its progress out of Pi's stderr channel.
    present=subprocess.run(['docker','image','inspect',image],capture_output=True)
    if present.returncode:
        subprocess.run(['docker','pull','--platform','linux/arm64',image],check=True)
    args=['docker','run','--rm','-i','--user',f'{os.getuid()}:{os.getgid()}',
          '-w','/home/tester','-v',f'{home}:/home/tester',
          '-e','MARSH_SELECTED_HOME=/home/tester','-e','SBX_CRED_OPENAI_MODE=apikey',
          '-e','SBX_CRED_GOOGLE_MODE=apikey','-e','SBX_CRED_ANTHROPIC_MODE=apikey',
          *[flag for source,target in mounts for flag in ('-v',f'{source}:{target}:ro')],image,
          '--offline','--mode','rpc','--no-session']
    process=subprocess.Popen(args,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
    events=queue.Queue();errors=[]
    def read_out():
        for line in process.stdout:
            try: events.put(json.loads(line))
            except json.JSONDecodeError: events.put({'invalid':line[:200]})
        events.put({'eof':True})
    def read_err():
        for line in process.stderr: errors.append(line)
    threading.Thread(target=read_out,daemon=True).start()
    threading.Thread(target=read_err,daemon=True).start()
    results={};observed=[]
    try:
        for index,command in enumerate(commands):
            process.stdin.write(json.dumps(dict(command,id=str(index)))+'\n')
            process.stdin.flush()
            while str(index) not in results:
                event=events.get(timeout=90)
                observed.append(event)
                if event.get('type')=='response':
                    assert event['success'],event
                    results[event['id']]=event.get('data')
                if event.get('eof'):raise RuntimeError('Pi exited before response: '+''.join(errors)[-2000:])
    finally:
        process.stdin.close()
        try:process.wait(timeout=20)
        except subprocess.TimeoutExpired:process.terminate();process.wait(timeout=20)
    assert process.returncode==0,(process.returncode,''.join(errors)[-2000:])
    assert not errors,''.join(errors)[-2000:]
    results['_events']=observed
    return results

def terminal_check(image, home):
    master,slave=pty.openpty()
    fcntl.ioctl(slave,termios.TIOCSWINSZ,struct.pack('HHHH',32,120,0,0))
    process=subprocess.Popen(['docker','run','--rm','-it','--user',f'{os.getuid()}:{os.getgid()}',
        '-w','/home/tester','-v',f'{home}:/home/tester',
        '-e','MARSH_SELECTED_HOME=/home/tester','-e','SBX_CRED_ANTHROPIC_MODE=apikey',
        '-e','TERM=xterm-256color','-e','COLORTERM=truecolor',image,'--offline','--no-session'],
        stdin=slave,stdout=slave,stderr=slave,start_new_session=True)
    os.close(slave);data=bytearray()
    def text():
        return re.sub(r'\x1b\[[0-?]*[ -/]*[@-~]','',data.decode(errors='replace'))
    def until(marker, timeout=45):
        deadline=time.monotonic()+timeout
        while marker.lower() not in text().lower():
            if time.monotonic()>deadline:raise AssertionError('PTY timeout: '+text()[-3000:])
            if select.select([master],[],[],1)[0]:
                try:chunk=os.read(master,65536)
                except OSError:chunk=b''
                if not chunk:raise AssertionError('PTY closed: '+text()[-3000:])
                data.extend(chunk)
    try:
        until('Pix UI')
        # Session-start UI can render before Pi finishes its own startup
        # dependency checks. Wait for the startup frame to stop changing.
        quiet_until=time.monotonic()+0.5
        settle_deadline=time.monotonic()+10
        while time.monotonic()<quiet_until and time.monotonic()<settle_deadline:
            if select.select([master],[],[],0.1)[0]:
                data.extend(os.read(master,65536))
                quiet_until=time.monotonic()+0.5
        # Exercise editing, arrows/backspace and bracketed paste, without sending
        # an ordinary user prompt (which would call a provider).
        os.write(master,b'drafX\x7ft\x1b[D\x1b[C\x15')
        os.write(master,b'\x1b[200~paste-probe\x1b[201~\x15')
        os.write(master,b'/pix-status\r')
        until('Execution: marsh owns')
        os.write(master,b'\x03\x04') # clear any draft, then native empty-editor exit
        deadline=time.monotonic()+15
        # Keep draining while Pi restores the terminal. Waiting without reading
        # can fill the PTY and deadlock a perfectly healthy shutdown.
        while process.poll() is None:
            if time.monotonic()>deadline:raise AssertionError('PTY did not exit: '+text()[-3000:])
            if select.select([master],[],[],0.1)[0]:
                try:chunk=os.read(master,65536)
                except OSError:chunk=b''
                if chunk:data.extend(chunk)
                else:process.wait(timeout=5)
        assert process.returncode==0,(process.returncode,text()[-2000:])
        print('PASS: real PTY startup, edit/paste/status UI and clean exit; no model prompt sent.')
    finally:
        os.close(master)
        if process.poll() is None:process.terminate();process.wait(timeout=20)

def main():
    image='pix-public-test:'+uuid.uuid4().hex[:12]
    # Tests never read or mount the user's real agent home or control scope.
    with tempfile.TemporaryDirectory(prefix='pix-public-runtime-',dir=os.environ.get('PIX_TEST_TMPDIR')) as temp:
        temp=Path(temp);kit=temp/'kit';home=temp/'home';home.mkdir()
        subprocess.run(['python3',str(ROOT/'marsh/prepare.py'),str(kit)],check=True)
        subprocess.run(['docker','buildx','build','--platform','linux/arm64','--load',
                        '--progress=plain','-t',image,'-f',str(kit/'pix.yaml'),str(kit)],check=True)
        (home/'.pi/agent').mkdir(parents=True)
        original='{"theme":"light","lastChangelogVersion":"1.2.0"}\n'
        (home/'.pi/agent/settings.json').write_text(original)
        commands=[{'type':'get_state'},{'type':'get_commands'},{'type':'get_available_models'}]
        public=rpc(image,home,commands)
        assert public['0']['model']['provider']=='openai' and public['0']['model']['id']=='gpt-6.1-sol'
        resources=public['1']['commands']
        assert len([c for c in resources if c.get('name','').startswith('skill:')])==37
        assert any(c.get('name')=='pix-status' for c in resources)
        assert any(c.get('name')=='output-style' for c in resources)
        # A mixin's skills, as marsh and stock SBX deliver them: links in a
        # read-only /opt/pix/kit-skills to skill folders elsewhere in the image.
        (temp/'team/hello').mkdir(parents=True);(temp/'kit-skills').mkdir()
        (temp/'team/hello/SKILL.md').write_text('---\nname: hello\ndescription: Team probe skill.\n---\nSay hello.\n')
        (temp/'kit-skills/hello').symlink_to('/opt/team-skills/hello')
        mixed=rpc(image,home,[{'type':'get_commands'}],
                  mounts=[(temp/'team','/opt/team-skills'),(temp/'kit-skills','/opt/pix/kit-skills')])
        skills=[c['name'] for c in mixed['0']['commands'] if c.get('name','').startswith('skill:')]
        assert len(skills)==38 and 'skill:hello' in skills,skills
        settings=home/'.pix-agent/agent/settings.json';snapshot=settings.read_bytes()
        assert (home/'.pi/agent/settings.json').read_text()==original
        assert json.loads(snapshot)['theme']=='kanagawa'
        assert (home/'.pix-agent/agent/themes/kanagawa.json').is_symlink()
        # Existing personal settings and bytes must survive later starts.
        settings.write_text('{"defaultProvider":"anthropic","defaultModel":"claude-sonnet-5-5","theme":"nord"}\n')
        saved=settings.read_bytes()
        second=rpc(image,home,[{'type':'get_state'}])
        assert second['0']['model']['id']=='claude-sonnet-5-5'
        assert settings.read_bytes()==saved
        plain=rpc(BASE,home,[{'type':'get_commands'}])
        assert not any(c.get('name','').startswith('skill:') or c.get('name')=='pix-status' for c in plain['0']['commands'])
        assert (home/'.pi/agent/settings.json').read_text()==original
        # The status command is an actual Pi command, not a mocked factory.
        status=rpc(image,home,[{'type':'prompt','message':'/pix-status'},{'type':'get_messages'}])
        assert any(e.get('type')=='extension_ui_request' and e.get('method')=='notify'
                   and 'Execution: marsh owns' in e.get('message','') for e in status['_events']),status['_events']
        assert status['1']['messages']==[], 'Display-only status must not change model context'
        styles=home/'.pix-agent/agent/context/output-styles'
        (styles/'sample.md').write_text('---\nname: Sample\n---\nUse concise prose.\n')
        rpc(image,home,[{'type':'prompt','message':'/output-style sample'}])
        assert (styles/'active').read_text().strip()=='sample'
        rpc(image,home,[{'type':'prompt','message':'/output-style off'}])
        assert not (styles/'active').exists()
        terminal_check(image,home)
        verified=subprocess.run(['docker','run','--rm','--entrypoint','python3',image,
            '/usr/local/share/licenses/pix/verify-public.py'],capture_output=True,text=True)
        assert verified.returncode==0,verified.stderr
        staged=subprocess.run(['docker','run','--rm','--entrypoint','ls',image,'/usr/share/sandbox/kit'],
            capture_output=True,text=True,check=True).stdout.split()
        assert staged==['pix'],staged
        tampered=subprocess.run(['docker','run','--rm','--user','0','--entrypoint','/bin/sh',image,'-c',
            'printf tampered >> /opt/pix/package/instructions.md; python3 /usr/local/share/licenses/pix/verify-public.py'],
            capture_output=True,text=True)
        assert tampered.returncode!=0 and 'differs' in tampered.stderr,tampered.stderr
        print('PASS: actual derivative notice/public-payload checks and tampered-payload negative control.')
        print('PASS: 37 public skills plus mixin skills from /opt/pix/kit-skills, UI/output-style loaded by upstream Pi; separate home, explicit default, settings preservation and stock-Pi isolation. Zero model/MCP calls.')
    print('Built test image retained for inspection:',image)

if __name__=='__main__':main()
