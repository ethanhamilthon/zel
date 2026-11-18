"""Shared loopback provider, isolated workspaces and PTY evidence capture."""

import argparse
import codecs
import fcntl
import json
import os
from pathlib import Path
import pty
import select
import struct
import subprocess
import sys
import tempfile
import termios
import threading
import time
import traceback
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Report:
    def __init__(self, path):
        self.path = path
        self.path.write_text("")
        self.failures = 0
        self.passes = 0

    def check(self, ok, label, detail=""):
        line = f"{'PASS' if ok else 'FAIL'} {label}"
        print(line, flush=True)
        with self.path.open("a") as f:
            f.write(line + "\n")
            if not ok and detail:
                f.write(str(detail) + "\n")
        self.failures += not bool(ok)
        self.passes += bool(ok)
        return ok

    def require(self, ok, label, detail=""):
        if not self.check(ok, label, detail):
            raise StopCase


class StopCase(Exception):
    """A missing prerequisite stops this case, not the rest of the suite."""


def message(text):
    return {"type": "message", "role": "assistant",
            "content": [{"type": "output_text", "text": text}]}


def lua(code, ident):
    return {"type": "function_call", "name": "lua", "call_id": ident,
            "arguments": json.dumps({"code": code})}


def inputs(body):
    return body.get("input", [])


def user_texts(body):
    for item in inputs(body):
        if item.get("role") == "user":
            content = item.get("content", "")
            yield ("\n".join(p.get("text", "") for p in content)
                   if isinstance(content, list) else content)


def outputs(body):
    return {item["call_id"]: item["output"] for item in inputs(body)
            if item.get("type") == "function_call_output"}


class Fixture:
    def __init__(self, binary, root, out, report, respond):
        self.binary, self.out, self.report = binary, out, report
        self.check, self.require = report.check, report.require
        self.out.mkdir(parents=True, exist_ok=True)
        self.home, self.zel_home, self.cwd = (root / n for n in ("home", "config", "project"))
        self.home.mkdir()
        self.cwd.mkdir()
        self.env = dict(HOME=str(self.home), ZEL_HOME=str(self.zel_home),
                        PATH=f"{binary.parent}{os.pathsep}{os.defpath}",
                        TERM="xterm-256color", COLORTERM="truecolor", LANG="en_US.UTF-8",
                        FIXTURE_KEY="loopback-only")
        self.requests = []
        self.terms = []
        self.calls = 0
        fixture = self
        request_log = self.out / "requests.jsonl"
        request_log.write_text("")
        lock = threading.Lock()

        class Provider(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps({"data": [
                    {"id": m} for m in ("fixture", "second-model")]}).encode())

            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                with lock:
                    fixture.requests.append(body)
                    with request_log.open("a") as f:
                        f.write(json.dumps(body) + "\n")
                result = respond(body)
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()

                def emit(data):
                    self.wfile.write(("data: " + json.dumps(data) + "\n\n").encode())
                    self.wfile.flush()

                try:
                    for index, item in enumerate(result):
                        emit({"type": "response.output_item.added", "output_index": index, "item": item})
                        if item["type"] == "message":
                            text = item["content"][0]["text"]
                            for offset in range(0, len(text), 8):
                                emit({"type": "response.output_text.delta", "output_index": index,
                                      "delta": text[offset:offset + 8]})
                    emit({"type": "response.completed", "response": {"output": result,
                          "usage": {"input_tokens": 200, "output_tokens": 40}}})
                except (BrokenPipeError, ConnectionResetError):
                    pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Provider)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.connect = self.zel_home / "connect.json"

    def configure(self):
        self.zel_home.mkdir(exist_ok=True)
        self.connect.write_text(json.dumps({"provider": "fixture", "model": "fixture", "effort": "low",
            "providers": {"fixture": {"base_url": f"http://127.0.0.1:{self.server.server_port}/v1",
                                      "api_key": "$FIXTURE_KEY"}}}, indent=2))

    def run(self, *args, stdin=None, timeout=20, env=None):
        self.calls += 1
        p = subprocess.run([self.binary, *args], cwd=self.cwd,
                           env=dict(self.env, **(env or {})), text=True, capture_output=True,
                           input=stdin, timeout=timeout)
        with (self.out / "cli.log").open("a") as f:
            f.write(f"{self.calls}: {args!r} -> {p.returncode}\n{p.stdout}{p.stderr}\n")
        return p

    def term(self, name, *args):
        t = Term(self, name, args)
        self.terms.append(t)
        return t

    def close(self):
        try:
            for t in self.terms:
                t.close()
        finally:
            self.server.shutdown()
            self.server.server_close()
            self.thread.join()

class Term:
    COLS, ROWS = 120, 40

    def __init__(self, fixture, name, args):
        import pyte

        self.fixture, self.name = fixture, name
        self.master, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", self.ROWS, self.COLS, 0, 0))
        # Acquire a controlling terminal without preexec_fn in the threaded parent.
        launcher = "import fcntl, os, sys, termios; fcntl.ioctl(0, termios.TIOCSCTTY, 0); os.execv(sys.argv[1], sys.argv[1:])"
        try:
            self.p = subprocess.Popen([sys.executable, "-c", launcher, fixture.binary, *args],
                                      stdin=slave, stdout=slave, stderr=slave, cwd=fixture.cwd,
                                      env=fixture.env, start_new_session=True)
        finally:
            os.close(slave)
        self.screen = pyte.Screen(self.COLS, self.ROWS)
        self.stream = pyte.Stream(self.screen)
        self.decoder = codecs.getincrementaldecoder("utf8")("replace")
        self.raw = bytearray()

    def pump(self, seconds=.2):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            if select.select([self.master], [], [], .04)[0]:
                try:
                    data = os.read(self.master, 65536)
                except OSError:
                    return
                if not data:
                    return
                self.raw.extend(data)
                self.stream.feed(self.decoder.decode(data))

    def send(self, data, delay=.25):
        os.write(self.master, data.encode())
        self.pump(delay)

    def rows(self):
        return [''.join(self.screen.buffer[y][x].data or ' ' for x in range(self.COLS)).rstrip()
                for y in range(self.ROWS)]

    def view(self):
        return "\n".join(self.rows())

    def until(self, condition, label, timeout=10, required=True):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            self.pump(.1)
            if condition():
                return self.fixture.check(True, label)
            if self.p.poll() is not None:
                break
        self.shot(f"failed-{self.fixture.report.failures + 1:02}")
        check = self.fixture.require if required else self.fixture.check
        return check(False, label, self.view())

    def wait(self, text, label, **opts):
        return self.until(lambda: text in self.view(), label, **opts)

    def shot(self, name):
        from PIL import Image, ImageDraw, ImageFont

        path = self.fixture.out / f"{self.name}-{name}"
        path.with_suffix(".txt").write_text(self.view())
        for candidate in ("/System/Library/Fonts/Menlo.ttc", "DejaVuSansMono.ttf"):
            try:
                font = ImageFont.truetype(candidate, 15)
                break
            except OSError:
                continue
        else:
            font = ImageFont.load_default()
        names = dict(default="#ededed", black="#222222", red="#ff3b30", green="#75c379",
                     brown="#ddc779", blue="#7ea7db", magenta="#bd8acf", cyan="#7fc9c0", white="#ededed")
        image = Image.new("RGB", (self.COLS * 10 + 24, self.ROWS * 21 + 24), "#0a0a0a")
        draw = ImageDraw.Draw(image)
        for y in range(self.ROWS):
            for x in range(self.COLS):
                cell = self.screen.buffer[y][x]
                fg = names.get(cell.fg, '#' + cell.fg if len(cell.fg) == 6 else names['default'])
                draw.text((12 + x * 10, 12 + y * 21), cell.data, font=font, fill=fg)
        image.save(path.with_suffix(".png"))

    def close(self):
        if self.master is not None:
            try:
                if self.p.poll() is None:
                    self.send("\x11")
                    self.p.wait(2)
            except (OSError, subprocess.TimeoutExpired):
                self.p.kill()
                self.p.wait()
            finally:
                self.pump()
                os.close(self.master)
                self.master = None
                (self.fixture.out / f"{self.name}.pty").write_bytes(self.raw)
                (self.fixture.out / f"{self.name}-final.txt").write_text(self.view())
        return self.p.returncode


def suite(report_name, cases, respond):
    parser = argparse.ArgumentParser()
    parser.add_argument("binary", type=Path, help="freshly built zel binary (must be named zel)")
    parser.add_argument("outdir", type=Path, help="directory for check reports and evidence")
    args = parser.parse_args()
    binary = args.binary.resolve()
    if not binary.is_file() or not os.access(binary, os.X_OK) or binary.name != "zel":
        parser.error("binary must be an executable named zel (also used by Lua defaults on PATH)")
    args.outdir.mkdir(parents=True, exist_ok=True)
    report = Report(args.outdir / report_name)
    for case in cases:
        # Short roots keep Unix-domain socket paths under platform limits.
        with tempfile.TemporaryDirectory(prefix="z012-", dir="/tmp") as root:
            fixture = Fixture(binary, Path(root), args.outdir / case.__name__, report, respond)
            try:
                case(fixture)
            except StopCase:
                pass
            except Exception:
                report.check(False, f"{case.__name__}: unexpected exception", traceback.format_exc())
            finally:
                fixture.close()
    summary = f"{report.passes} passed, {report.failures} failed"
    print(summary)
    with report.path.open("a") as f:
        f.write(summary + "\n")
    return int(report.failures != 0)
