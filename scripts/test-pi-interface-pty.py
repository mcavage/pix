#!/usr/bin/env python3
"""Exercise the installed pi command in a private PTY without sending a model request."""
import fcntl
import json
import os
from pathlib import Path
import pty
import re
import select
import shutil
import struct
import subprocess
import tempfile
import termios
import time

repo = Path(__file__).resolve().parent.parent
command = json.loads(os.environ.get("PI_INTERFACE_COMMAND", '["pi"]'))
with tempfile.TemporaryDirectory(prefix="pix-interface-") as temporary:
    root = Path(temporary)
    agent = root / "agent"
    agent.mkdir()
    shutil.copy(repo / "settings.json", agent / "settings.json")
    shutil.copytree(repo / "themes", agent / "themes")
    work = root / "work"
    work.mkdir()
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 30, 100, 0, 0))
    env = dict(os.environ, PI_CODING_AGENT_DIR=str(agent), TERM="xterm-256color", COLORTERM="truecolor")
    args = command + ["--no-extensions", "--no-skills", "--no-context-files", "--no-prompt-templates",
                      "-e", str(repo / "extensions/help.ts"), "-e", str(repo / "extensions/theme-picker.ts"),
                      "--provider", "openai", "--model", "gpt-5.4"]
    child = subprocess.Popen(args, cwd=work, env=env, stdin=slave, stdout=slave, stderr=slave, start_new_session=True)
    captured = bytearray()

    def read_until(predicate, description, timeout=25):
        result = bytearray()
        until = time.monotonic() + timeout
        while time.monotonic() < until:
            if select.select([master], [], [], 0.1)[0]:
                try:
                    data = os.read(master, 65536)
                except OSError:
                    break
                result.extend(data)
                captured.extend(data)
                if b"\x1b]11;?" in data:
                    os.write(master, b"\x1b]11;rgb:2020/2020/2020\x07")
                if predicate(bytes(result)):
                    # A PTY read can end midway through a screen repaint. Consume
                    # the rest before sending the next key or inspecting state.
                    repaint_deadline = time.monotonic() + 0.5
                    while time.monotonic() < repaint_deadline and select.select([master], [], [], 0.1)[0]:
                        try:
                            data = os.read(master, 65536)
                        except OSError:
                            break
                        if not data:
                            break
                        result.extend(data)
                        captured.extend(data)
                    return bytes(result)
            if child.poll() is not None:
                break
        clean = re.sub(rb"\x1b\[[0-?]*[ -/]*[@-~]", b"", bytes(result))
        raise AssertionError(f"Timed out: {description}\n{clean[-2500:].decode(errors='replace')}")

    try:
        startup = read_until(lambda b: b"Pix \xc2\xb7 Ask anything" in b and b"medium" in b, "quiet Pix welcome")
        for hidden in [b"[Context]", b"[Skills]", b"[Extensions]", b"[Themes]", b"full startup help", b"Pi can explain"]:
            assert hidden not in startup, f"startup inventory leaked: {hidden!r}"
        assert b"\x1b[48;2;40;42;54m" in startup, "actual CLI missed the background patch"
        assert "\x1b[38;2;189;147;249m───".encode() in startup, "medium border does not match Dracula accent"
        os.write(master, b"/theme\r")
        read_until(lambda b: b"Previewing the whole interface" in b, "theme overlay")
        os.write(master, b"\x1b[B\x1b[B")
        previewed = read_until(lambda b: "\x1b[38;2;30;102;245m───".encode() in b, "arrow-key preview updates the real editor")
        assert b"\x1b[48;2;239;241;245m" in previewed
        assert json.loads((agent / "settings.json").read_text())["theme"] == "dracula", "preview saved prematurely"
        os.write(master, b"\x1b")
        read_until(lambda b: "\x1b[38;2;189;147;249m───".encode() in b, "Escape restores original editor")
        assert json.loads((agent / "settings.json").read_text())["theme"] == "dracula"
        os.write(master, b"/theme\r")
        read_until(lambda b: b"Previewing the whole interface" in b, "reopen theme overlay")
        os.write(master, b"\x1b[B\x1b[B")
        read_until(lambda b: "\x1b[38;2;30;102;245m───".encode() in b, "second live preview")
        os.write(master, b"\r")
        switched = read_until(lambda b: b"Theme: Catppuccin Latte" in b, "commit the previewed theme")
        assert json.loads((agent / "settings.json").read_text())["theme"] == "catppuccin-latte"
        assert b"\x1b[48;2;239;241;245m" in switched
        assert b"\x1b[38;2;76;79;105m" in switched
        os.write(master, b"/help\r")
        read_until(lambda b: b"getting-started" in b, "theme-aware help extension")
        os.write(master, b"\x04")
        read_until(lambda b: b"\x1b[?2004l" in b, "terminal restoration")
        deadline = time.monotonic() + 10
        while child.poll() is None and time.monotonic() < deadline:
            if select.select([master], [], [], 0.1)[0]:
                try:
                    captured.extend(os.read(master, 65536))
                except OSError:
                    break
        child.wait(timeout=1)
        assert child.returncode == 0, child.returncode
        assert termios.tcgetattr(slave)[3] & termios.ICANON, "terminal remained in raw mode"
        print("Installed pi PTY: quiet welcome, themed medium border, live whole-interface preview, Escape restoration, Enter commit, /help, and clean exit passed.")
    finally:
        if child.poll() is None:
            child.terminate()
            try:
                child.wait(timeout=2)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait(timeout=2)
        os.close(master)
        os.close(slave)
        if os.environ.get("PIX_INTERFACE_CAPTURE"):
            Path(os.environ["PIX_INTERFACE_CAPTURE"]).write_bytes(captured)
