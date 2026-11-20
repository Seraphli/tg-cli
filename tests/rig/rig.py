"""Real-cc rig for hook-path scenarios that need a slow Telegram: python3 rig.py SCENARIO TG_CLI_BIN [CC_BIN]
SCENARIO: clear (done line + /clear at 3 s Telegram latency), perm (turn 2's permission prompt), week, five (5 h rate limit), all (in parallel).
The bot and cc are real; Telegram and the model API are in-process fakes. One PASS/FAIL line per check, then the seconds."""
import json, os, re, shutil, socket, subprocess, sys, threading, time, urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

REPO = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
HOME = os.path.expanduser("~")
LATENCY = 3.0
LIMIT = {"retry-after": "3600", "x-should-retry": "false", "anthropic-ratelimit-unified-status": "rejected", "anthropic-ratelimit-unified-overage-status": "rejected"}


def sse(name, **d):
    return ("event: %s\ndata: %s\n\n" % (name, json.dumps(dict(type=name, **d)))).encode()


def text(i, t):
    return sse("content_block_start", index=i, content_block={"type": "text", "text": ""}) + sse("content_block_delta", index=i, delta={"type": "text_delta", "text": t}) + sse("content_block_stop", index=i)


def tool(i, name, inp):
    return sse("content_block_start", index=i, content_block={"type": "tool_use", "id": "toolu_%s%d" % (name, time.time_ns()), "name": name, "input": {}}) + sse("content_block_delta", index=i, delta={"type": "input_json_delta", "partial_json": json.dumps(inp)}) + sse("content_block_stop", index=i) + sse("message_delta", delta={"stop_reason": "tool_use", "stop_sequence": None}, usage={"output_tokens": 5}) + sse("message_stop")


def reply_json(h, code, obj, headers=()):
    b = json.dumps(obj).encode()
    h.send_response(code)
    for k, v in (("content-type", "application/json"), ("content-length", str(len(b))), *headers):
        h.send_header(k, v)
    h.end_headers()
    h.wfile.write(b)


class TG(BaseHTTPRequestHandler):
    """Fake Telegram Bot API: every method but getMe and getUpdates takes LATENCY seconds; every call is logged."""
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("content-length") or 0))
        m = self.path.rsplit("/", 1)[-1]
        p = json.loads(raw) if raw[:1] == b"{" else {}
        e = {"t": time.time(), "m": m, "p": p, "id": None}
        if m == "getUpdates":
            time.sleep(min(float(p.get("timeout", 10) or 10), 10))
            return reply_json(self, 200, {"ok": True, "result": []})
        if m == "getMe":
            return reply_json(self, 200, {"ok": True, "result": {"id": 123456, "is_bot": True, "first_name": "rig", "username": "rig_bot"}})
        time.sleep(LATENCY)
        res = True
        if m.startswith(("send", "edit")):
            e["id"] = int(p.get("message_id") or time.time_ns() % 10**9)
            res = {"message_id": e["id"], "date": int(time.time()), "chat": {"id": int(p.get("chat_id", 424242)), "type": "private"}, "text": str(p.get("text", ""))}
        self.server.log.append(e)
        reply_json(self, 200, {"ok": True, "result": res})


class API(BaseHTTPRequestHandler):
    """Fake model API: the prompt word picks the turn (GOTURN text only, READTURN a Read call, BASHTURN a Bash call); `limit` answers 429."""
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("content-length") or 0)))
        model, msgs, srv = body.get("model", ""), body.get("messages", []), self.server
        if "haiku" not in model and srv.limit:
            return reply_json(self, 429, {"type": "error", "error": {"type": "rate_limit_error", "message": "fake rate limit"}}, [(k, v.replace("RESET", str(int(time.time()) + srv.reset))) for k, v in srv.limit.items()])
        if not body.get("stream"):
            return reply_json(self, 200, {"id": "msg_1", "type": "message", "role": "assistant", "model": model, "content": [{"type": "text", "text": "ok"}], "stop_reason": "end_turn", "stop_sequence": None, "usage": {"input_tokens": 5, "output_tokens": 1}})
        time.sleep(srv.delay)
        i, turn = next(((i, k) for i in range(len(msgs) - 1, -1, -1) if msgs[i]["role"] == "user" for k in ("GOTURN", "READTURN", "BASHTURN") if k in json.dumps(msgs[i])), (0, ""))
        done = '"tool_result"' in json.dumps(msgs[i + 1:])
        if turn == "READTURN" and not done:
            out = text(0, "Reading the file.") + tool(1, "Read", {"file_path": srv.cwd + "/notes.txt"})
        elif turn == "BASHTURN" and not done:
            out = text(0, "Running the command.") + tool(1, "Bash", {"command": "touch ran.txt", "description": "Create the marker"})
        else:
            out = text(0, "ok") + sse("message_delta", delta={"stop_reason": "end_turn", "stop_sequence": None}, usage={"output_tokens": 5}) + sse("message_stop")
        out = sse("message_start", message={"id": "msg_1", "type": "message", "role": "assistant", "model": model, "content": [], "stop_reason": None, "stop_sequence": None, "usage": {"input_tokens": 10, "output_tokens": 1}}) + out
        self.send_response(200)
        for k, v in (("content-type", "text/event-stream"), ("transfer-encoding", "chunked")):
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(b"%x\r\n" % len(out) + out + b"\r\n0\r\n\r\n")
        srv.answered = time.time()


def serve(handler):
    srv = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


class Rig:
    def __init__(self, label, tgbin, ccbin, limit=None, reset=0, oauth=False, perm=False, delay=0):
        self.label, self.results = label, []
        self.srv = "rig%d" % os.getpid()
        self.dir = "%s/.tg-cli-test/%s" % (HOME, self.srv)
        self.cfg, self.cwd, ccc = self.dir + "/tgcfg", self.dir + "/cwd", self.dir + "/claude"
        for d in (self.cfg, self.cwd, ccc):
            os.makedirs(d)
        open(self.cwd + "/notes.txt", "w").write("notes\n")
        self.tgs, self.api = serve(TG), serve(API)
        self.tgs.log, self.api.limit, self.api.reset, self.api.cwd, self.api.delay, self.api.answered = [], limit, reset, self.cwd, delay, 0
        with socket.socket() as s:
            s.bind(("127.0.0.1", 0))
            self.port = s.getsockname()[1]
        json.dump({"botToken": "123456:FAKE-RIG", "pairingAllow": {"ids": ["424242"], "defaultChatId": "424242"}, "port": self.port, "nameRouteMap": {"rig": {"chatId": -1001234567890}}}, open(self.cfg + "/credentials.json", "w"))
        key = "local-fake-key-0123456789abcdef"
        json.dump({"hasCompletedOnboarding": True, "hasInitOnboardingBeenShown": True, "numStartups": 100, "autoUpdates": False, "customApiKeyResponses": {"approved": [key[-20:]], "rejected": []}, "projects": {self.cwd: {"hasTrustDialogAccepted": True}}}, open(ccc + "/.claude.json", "w"))
        hooks = json.load(open(REPO + "/cmd/config/cc.json"))["hooks"]
        for ents in hooks.values():
            for e in ents:
                for h in e["hooks"]:
                    h["command"] = h["command"].replace("HOOK_CMD", "%s --config-dir %s hook --port %d" % (tgbin, self.cfg, self.port))
        json.dump({"hooks": hooks}, open(ccc + "/settings.json", "w"))
        if oauth:
            json.dump({"claudeAiOauth": {"accessToken": "fake-access", "refreshToken": "fake-refresh", "expiresAt": int(time.time() * 1000) + 30 * 24 * 3600 * 1000, "scopes": ["user:inference", "user:profile"], "subscriptionType": "max", "rateLimitTier": "default_claude_max_20x"}}, open(ccc + "/.credentials.json", "w"))
        self.tmux("new-session", "-d", "-s", "rig", "-x", "205", "-y", "40", "-c", self.cwd)
        self.pane_id = self.tmux("list-panes", "-t", "rig", "-F", "#{pane_id}").strip()
        self.sock = self.tmux("display-message", "-p", "#{socket_path}").strip()
        json.dump({"seed-session-id": {"tmux_target": "%s@%s" % (self.pane_id, self.sock), "cwd": self.cwd, "backend": "cc", "name": "rig"}}, open(self.cfg + "/sessions.json", "w"))
        tgurl = "http://127.0.0.1:%d" % self.tgs.server_port
        self.bot = subprocess.Popen([tgbin, "--config-dir", self.cfg, "bot", "--port", str(self.port), "--tmux-server", self.srv, "--debug"], env=dict(os.environ, TG_CLI_TELEGRAM_URL=tgurl), stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
        self.wait(lambda: self.get("/session/idle") is not None, 15)
        pre = "unset ANTHROPIC_BASE_URL ANTHROPIC_AUTH_TOKEN ANTHROPIC_API_KEY ANTHROPIC_MODEL ANTHROPIC_DEFAULT_OPUS_MODEL ANTHROPIC_DEFAULT_SONNET_MODEL ANTHROPIC_DEFAULT_HAIKU_MODEL CLAUDE_CODE_SUBAGENT_MODEL CLAUDE_CODE_EFFORT_LEVEL; BROWSER=none DISABLE_AUTOUPDATER=1 CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 DISABLE_TELEMETRY=1 ANTHROPIC_BASE_URL=http://127.0.0.1:%d CLAUDE_CONFIG_DIR=%s" % (self.api.server_port, ccc)
        pre += "" if oauth else " ANTHROPIC_API_KEY=" + key
        self.tmux("send-keys", "-t", self.pane_id, "-l", "%s %s --model sonnet %s--setting-sources user --no-chrome --tools \"Bash,Read\"" % (pre, ccbin, "--permission-mode default " if perm else ""))
        time.sleep(1)
        self.tmux("send-keys", "-t", self.pane_id, "Enter")
        assert self.wait(lambda: "❯" in self.pane(), 30), "cc did not reach its prompt"
        time.sleep(1.5)

    def tmux(self, *a):
        return subprocess.run(["tmux", "-u", "-L", self.srv, "-f", "/dev/null", *a], capture_output=True, text=True).stdout

    def pane(self):
        return self.tmux("capture-pane", "-p", "-t", self.pane_id)

    def get(self, path):
        try:
            return urllib.request.urlopen("http://127.0.0.1:%d%s" % (self.port, path), timeout=2).read()
        except OSError:
            return None

    def type(self, s):
        self.tmux("send-keys", "-t", self.pane_id, "-l", s)
        time.sleep(0.5)
        t = time.time()
        self.tmux("send-keys", "-t", self.pane_id, "Enter")
        return t

    def wait(self, pred, timeout):
        end = time.time() + timeout
        while time.time() < end:
            if pred():
                return time.time()
            time.sleep(0.05)

    def sent(self, method, needle):
        return [e for e in self.tgs.log if e["m"].startswith(method) and needle in json.dumps(e["p"], ensure_ascii=False)]

    def botlog(self):
        return open(self.cfg + "/bot.log", errors="replace").read()

    def check(self, name, ok, detail=""):
        self.results.append(bool(ok))
        print("%s %s %s" % ("PASS" if ok else "FAIL", name, detail))

    def close(self):
        ev = "%s/.tg-cli-test/evidence-busy/rig/%s" % (HOME, self.label)
        os.makedirs(ev, exist_ok=True)
        open(ev + "/pane.txt", "w").write(self.pane())
        open(ev + "/tg.log", "w").write("".join("%.3f %s %s id=%s\n" % (e["t"], e["m"], json.dumps(e["p"], ensure_ascii=False)[:700], e["id"]) for e in self.tgs.log))
        subprocess.run("cp %s/*.log %s/" % (self.cfg, ev), shell=True)
        self.tmux("kill-server")
        self.bot.terminate()
        self.bot.wait(10)
        time.sleep(1)
        shutil.rmtree(self.dir, ignore_errors=True)
        subprocess.run(["rm", "-f", self.sock])


def clear(r):
    r.type("GOTURN")
    td = r.wait(lambda: "· done" in r.pane(), 30)
    r.check("TC29 done line within 1 s of the model's answer, Telegram at 3 s", td and td - r.api.answered < 1, "%.2f s" % (td - r.api.answered) if td else "no done line")
    time.sleep(0.3)
    r.type("/clear")
    r.check("TC32 SessionEnd notification sent", r.wait(lambda: r.sent("send", "Session Ended"), 20))


def perm(r):
    r.type("READTURN")
    r.wait(lambda: "· done" in r.pane(), 30)
    r.type("BASHTURN")
    r.wait(lambda: "Do you want to proceed" in r.pane(), 30)
    sent = r.wait(lambda: r.sent("send", "Allow"), 30)
    r.check("TC31 TC34 permission message with Allow/Deny sent", sent)
    if not sent:
        return
    mid = r.sent("send", "Allow")[0]["id"]
    n, quiet, end = -1, 0, time.time() + 90
    while quiet < 8 and time.time() < end:
        quiet = quiet + 1 if n == len(r.tgs.log) else 0
        n = len(r.tgs.log)
        time.sleep(1)
    urllib.request.urlopen(urllib.request.Request("http://127.0.0.1:%d/test/raw-callback" % r.port, json.dumps({"unique": "perm", "data": "allow", "chat_id": -1001234567890, "sender_id": 424242, "msg_id": mid}).encode(), {"Content-Type": "application/json"}))
    r.check("TC31 tap on Allow runs the command", r.wait(lambda: os.path.exists(r.cwd + "/ran.txt"), 20))


def week(r):
    r.type("GOTURN")
    r.check("TC33 weekly popup shown", r.wait(lambda: "❯ 1. Stop and wait for limit to reset" in r.pane(), 15))
    r.check("TC33 weekly popup closed by one key", r.wait(lambda: "Stop and wait for limit to reset" not in r.pane(), 15))
    r.check("TC33 rate-limit notification sent", r.wait(lambda: r.sent("send", "Rate limit detected"), 15))


def five(r):
    r.type("GOTURN Stop and wait for limit to reset")
    r.check("TC33 StopFailure reached the bot", r.wait(lambda: "[StopFailure]" in r.botlog(), 15))
    time.sleep(8)
    r.check("TC33 5 h window: no key reaches the pane", not re.search(r"^❯\s*1\s*$", r.pane(), re.M) and not r.sent("send", "Rate limit detected"))


SCENARIOS = {
    "clear": (clear, {"delay": 6}),
    "perm": (perm, {"perm": True}),
    "week": (week, {"oauth": True, "limit": dict(LIMIT, **{"anthropic-ratelimit-unified-representative-claim": "seven_day", "anthropic-ratelimit-unified-reset": "RESET"}), "reset": 5 * 86400}),
    "five": (five, {"oauth": True, "limit": dict(LIMIT, **{"anthropic-ratelimit-unified-representative-claim": "five_hour", "anthropic-ratelimit-unified-reset": "RESET"}), "reset": 3600}),
}

if __name__ == "__main__":
    name, tgbin = sys.argv[1], sys.argv[2]
    t0 = time.time()
    if name == "all":
        codes = [p.wait() for p in [subprocess.Popen([sys.executable, __file__, s, *sys.argv[2:]]) for s in SCENARIOS]]
        print("all %s: %d scenarios, %d failed, %.1f s wall" % (os.path.basename(tgbin), len(codes), sum(c != 0 for c in codes), time.time() - t0))
        sys.exit(1 if any(codes) else 0)
    fn, kw = SCENARIOS[name]
    r = Rig("%s-%s-%d" % (name, os.path.basename(tgbin), os.getpid()), tgbin, sys.argv[3] if len(sys.argv) > 3 else HOME + "/.local/bin/claude", **kw)
    try:
        fn(r)
    finally:
        r.close()
    print("%s %s: %d checks, %d failed, %.1f s" % (name, os.path.basename(tgbin), len(r.results), r.results.count(False), time.time() - t0))
    sys.exit(1 if False in r.results else 0)
