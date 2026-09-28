#!/usr/bin/env python3
"""End-to-end test: real Mosquitto + nab-core --simulate + nab-service.

Covers the MQTT contract (docs/protocol-v1.md): execution, results,
deduplication/replay, expiration (on arrival and while queued), cancel,
retained command refusal, bounds validation, broker restart and core restart,
plus the HTTP side of nab-service (setup with physical presence, CSRF,
authenticated actions, settings persistence, /healthz).

Requirements: mosquitto, mosquitto_pub, mosquitto_sub, cargo, go.
Overrides (commands, split like a shell line, so emulator wrappers work):
  NAB_CORE_BIN="qemu-arm-static -L /sysroot /sysroot/usr/bin/nab-core"
  NAB_SERVICE_BIN="/sysroot/usr/bin/nab-service"
  MOSQUITTO=/usr/sbin/mosquitto
Without overrides, native debug builds of core/ and services/ are made.
"""

import http.client
import json
import os
import shlex
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PREFIX = "nabos/v1"


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def which(name, *extra):
    for c in (shutil.which(name), *extra):
        if c and os.path.exists(c):
            return c
    sys.exit(f"missing {name}")


class Harness:
    def __init__(self):
        self.tmp = tempfile.mkdtemp(prefix="nabos-e2e-")
        self.mqtt_port = free_port()
        self.http_port = free_port()
        self.procs = {}
        self.log = []  # (time, retained, topic, payload)
        self.lock = threading.Lock()
        self.failures = 0
        self.mosquitto = os.environ.get("MOSQUITTO") or which("mosquitto", "/usr/sbin/mosquitto")
        self.pub = which("mosquitto_pub")
        self.sub = which("mosquitto_sub")

    # processes

    def build(self):
        core = os.environ.get("NAB_CORE_BIN")
        if not core:
            subprocess.run(["cargo", "build", "--quiet", "--manifest-path", f"{ROOT}/core/Cargo.toml"], check=True)
            core = f"{ROOT}/core/target/debug/nab-core"
        service = os.environ.get("NAB_SERVICE_BIN")
        if not service:
            service = f"{self.tmp}/nab-service"
            subprocess.run(["go", "build", "-o", service, "./cmd/nab-service"], cwd=f"{ROOT}/services", check=True)
        self.core_cmd, self.service_cmd = shlex.split(core), shlex.split(service)
        for cmd in (self.core_cmd, self.service_cmd):
            out = subprocess.run(cmd + ["--version"], capture_output=True, text=True, timeout=60)
            if out.returncode != 0:
                sys.exit(f"{shlex.join(cmd)} --version failed: {out.stderr}")
            print(f"using {shlex.join(cmd)} ({out.stdout.strip()})")

    def spawn(self, name, args, env=None):
        logf = open(f"{self.tmp}/{name}.log", "ab")
        self.procs[name] = subprocess.Popen(args, env={**os.environ, **(env or {})}, stdout=logf, stderr=subprocess.STDOUT)

    def stop(self, name, sig=signal.SIGTERM):
        p = self.procs.pop(name, None)
        if p and p.poll() is None:
            p.send_signal(sig)
            try:
                p.wait(10)
            except subprocess.TimeoutExpired:
                p.kill()
                p.wait()

    def start_broker(self):
        conf = f"{self.tmp}/mosquitto.conf"
        with open(conf, "w") as f:
            f.write(f"listener {self.mqtt_port} 127.0.0.1\nallow_anonymous true\npersistence false\n")
        self.spawn("mosquitto", [self.mosquitto, "-c", conf])
        self.wait_for(lambda: self.port_open(self.mqtt_port), 10, "broker listening")

    def start_sub_reader(self):
        p = subprocess.Popen([self.sub, "-h", "127.0.0.1", "-p", str(self.mqtt_port), "-V", "mqttv5", "-q", "1",
                              "-t", f"{PREFIX}/#", "-F", "%r|%t|%p"], stdout=subprocess.PIPE, text=True, bufsize=1)
        self.procs["sub"] = p

        def reader():
            for line in p.stdout:
                line = line.rstrip("\n")
                if not line:
                    continue
                retained, topic, payload = line.split("|", 2)
                with self.lock:
                    self.log.append((time.time(), retained == "1", topic, payload))

        threading.Thread(target=reader, daemon=True).start()
        time.sleep(0.5)

    def start_core(self):
        sounds = f"{ROOT}/assets/sounds"
        chors = f"{ROOT}/assets/choreographies"
        self.spawn("core", self.core_cmd + ["--simulate"], {
            "NABOS_MQTT_PORT": str(self.mqtt_port), "NABOS_SOUNDS_DIRS": sounds,
            "NABOS_CHOREOGRAPHIES_DIRS": chors, "NABOS_SIM_AUDIO_MS": "200", "NABOS_LOG": "debug"})

    def start_service(self):
        data = f"{self.tmp}/data"
        os.makedirs(data, exist_ok=True)
        cfg = f"{data}/config.json"
        if not os.path.exists(cfg):
            # Never asleep, no chime: time-of-day independent test.
            with open(cfg, "w") as f:
                json.dump({"version": 1, "locale": "fr_FR", "timezone": "Europe/Paris", "volume": 70,
                           "clock": {"chime": False, "sleep_sounds": False, "wakeup": {"hour": 0, "min": 0},
                                     "sleep": {"hour": 0, "min": 0},
                                     "days": [{"wakeup": {"hour": 0, "min": 0}, "sleep": {"hour": 0, "min": 0}}] * 7},
                           "weather": {"unit": "celsius", "animation": "weather_and_rain", "frequency": 0},
                           "home_assistant": {"port": 1883, "discovery_prefix": "homeassistant"}}, f)
        self.spawn("service", self.service_cmd, {
            "NABOS_MQTT_PORT": str(self.mqtt_port), "NABOS_HTTP_ADDR": f"127.0.0.1:{self.http_port}",
            "NABOS_DATA_DIR": data, "NABOS_LVA_UNIT": "", "NABOS_TIMESYNC_FILE": "none",
            "NABOS_WEATHER_URL": "http://127.0.0.1:9/forecast", "NABOS_GEOCODING_URL": "http://127.0.0.1:9/search",
            "NABOS_NET_PROBE": "127.0.0.1:9", "NABOS_LOG": "debug", "NABOS_VERSION": "v0.0.1"})
        self.wait_for(lambda: self.port_open(self.http_port), 15, "service listening")

    def port_open(self, port):
        with socket.socket() as s:
            return s.connect_ex(("127.0.0.1", port)) == 0

    # MQTT helpers

    def publish(self, topic, payload, retain=False):
        args = [self.pub, "-h", "127.0.0.1", "-p", str(self.mqtt_port), "-V", "mqttv5", "-q", "1", "-t", topic, "-m", payload]
        if retain:
            args.append("-r")
        return subprocess.run(args, capture_output=True).returncode == 0

    def command(self, id, action, args=None, ttl=60, retain=False, expires_at=None):
        env = {"v": 1, "id": id, "expires_at": int(expires_at if expires_at is not None else time.time() + ttl),
               "action": action, "args": args or {}}
        return self.publish(f"{PREFIX}/core/cmd", json.dumps(env), retain)

    def messages(self, topic, since=0.0):
        with self.lock:
            return [(t, r, json.loads(p) if p.startswith("{") else p) for t, r, tp, p in self.log if tp == topic and t >= since]

    def results(self, id):
        return [m for _, _, m in self.messages(f"{PREFIX}/core/result") if isinstance(m, dict) and m.get("id") == id]

    def wait_for(self, cond, timeout, what):
        end = time.time() + timeout
        while time.time() < end:
            v = cond()
            if v:
                return v
            time.sleep(0.05)
        raise AssertionError(f"timeout waiting for {what}")

    def result(self, id, timeout=10):
        return self.wait_for(lambda: self.results(id), timeout, f"result of {id}")[0]

    def state_since(self, t, state, timeout=10):
        return self.wait_for(lambda: [m for _, _, m in self.messages(f"{PREFIX}/core/state", t) if m["state"] == state],
                             timeout, f"state {state}")

    def online_since(self, t, value="online", timeout=15):
        return self.wait_for(lambda: [m for _, _, m in self.messages(f"{PREFIX}/core/availability", t) if m == value],
                             timeout, f"core {value}")

    # HTTP helpers

    def http(self, method, path, form=None, cookie=None, origin=True):
        c = http.client.HTTPConnection("127.0.0.1", self.http_port, timeout=90)
        headers = {}
        body = None
        if form is not None:
            body = urllib.parse.urlencode(form)
            headers["Content-Type"] = "application/x-www-form-urlencoded"
        if cookie:
            headers["Cookie"] = cookie
        if origin and method == "POST":
            headers["Origin"] = f"http://127.0.0.1:{self.http_port}"
        c.request(method, path, body, headers)
        r = c.getresponse()
        data = r.read().decode()
        return r.status, dict(r.getheaders()), data

    # reporting

    def check(self, name, fn):
        try:
            fn()
            print(f"ok   {name}")
        except Exception as e:  # noqa: BLE001 - report every failing scenario
            self.failures += 1
            print(f"FAIL {name}: {e}")

    def cleanup(self):
        for name in ("service", "core", "sub", "mosquitto"):
            self.stop(name)


def main():
    if not __debug__:
        sys.exit("integration checks require Python assertions; do not use -O")
    h = Harness()
    h.build()
    try:
        run(h)
    except Exception:
        h.failures += 1  # Keep logs for startup failures as well as failed checks.
        raise
    finally:
        h.cleanup()
        if h.failures:
            for name in ("core", "service", "mosquitto"):
                path = f"{h.tmp}/{name}.log"
                if os.path.exists(path):
                    print(f"--- {name}.log (tail)")
                    print("".join(open(path, errors="replace").readlines()[-40:]))
        else:
            shutil.rmtree(h.tmp, ignore_errors=True)
    print(f"core: {shlex.join(h.core_cmd)}\nservice: {shlex.join(h.service_cmd)}\nbroker: {h.mosquitto}")
    print("all end-to-end checks passed" if not h.failures else f"{h.failures} end-to-end check(s) failed")
    sys.exit(1 if h.failures else 0)


def run(h):
    h.start_broker()
    h.start_sub_reader()
    t0 = time.time()
    h.start_core()
    h.online_since(t0)
    h.state_since(t0, "idle")

    def play_ok():
        t = time.time()
        h.command("p1", "play", {"sequence": [{"audio": ["system/abort.wav"], "choreography": "system/rfid.chor"}]})
        assert h.result("p1")["status"] == "ok"
        h.state_since(t, "playing")
        h.state_since(t, "idle")

    def duplicate_not_replayed():
        h.command("p1", "play", {"sequence": [{"audio": ["system/abort.wav"]}]})
        h.wait_for(lambda: len(h.results("p1")) == 2, 5, "second answer")
        assert h.results("p1")[1]["status"] == "duplicate", h.results("p1")

    def expired_on_arrival():
        h.command("old", "play", {"sequence": [{"audio": ["system/abort.wav"]}]}, expires_at=time.time() - 30)
        assert h.result("old")["status"] == "expired"

    def invalid_rejected():
        h.command("trav", "play", {"sequence": [{"audio": ["../../etc/passwd"]}]})
        assert h.result("trav")["status"] == "rejected"
        h.command("far", "sleep", expires_at=time.time() + 7 * 86400)
        assert h.result("far")["status"] == "rejected"
        h.publish(f"{PREFIX}/core/cmd", "not json")
        h.command("ears", "ears", {"left": 99})
        assert h.result("ears")["status"] == "rejected"

    def retained_refused_and_cleared():
        h.command("ret", "play", {"sequence": [{"audio": ["system/abort.wav"]}]}, retain=True)
        r = h.result("ret")
        assert r["status"] == "rejected" and r["error"] == "retained", r
        out = subprocess.run([h.sub, "-h", "127.0.0.1", "-p", str(h.mqtt_port), "-t", f"{PREFIX}/core/cmd",
                              "--retained-only", "-W", "2"], capture_output=True, text=True)
        assert out.stdout.strip() == "", f"retained command left on broker: {out.stdout!r}"

    long_seq = {"sequence": [{"audio": ["system/abort.wav"] * 10}]}

    def cancel_running_and_queued():
        h.command("long", "play", long_seq)
        h.command("queued", "play", long_seq)
        time.sleep(0.5)
        h.command("c1", "cancel", {"target": "queued"})
        assert h.result("c1")["status"] == "ok"
        assert h.result("queued")["status"] == "canceled"
        h.command("c2", "cancel", {"target": "long"})
        assert h.result("c2")["status"] == "ok"
        assert h.result("long")["status"] == "canceled"
        h.command("c3", "cancel")
        assert h.result("c3")["error"] == "not_playing"

    def expires_while_queued():
        h.command("blocker", "play", long_seq)
        h.command("short", "play", {"sequence": [{"audio": ["system/abort.wav"]}]}, ttl=1)
        assert h.result("short", timeout=5)["status"] == "expired"
        assert h.result("blocker")["status"] == "ok"

    for name, fn in [("play executes and returns to idle", play_ok),
                     ("duplicate id is not executed again", duplicate_not_replayed),
                     ("expired command refused", expired_on_arrival),
                     ("traversal, bounds and garbage refused", invalid_rejected),
                     ("retained command refused and cleared", retained_refused_and_cleared),
                     ("cancel running and queued commands", cancel_running_and_queued),
                     ("command expires while queued", expires_while_queued)]:
        h.check(name, fn)

    # Service
    h.start_service()
    cookie = {}

    def healthz_and_setup():
        h.wait_for(lambda: h.http("GET", "/healthz")[0] == 200, 15, "healthz 200")
        status, headers, _ = h.http("GET", "/")
        assert status == 303 and headers["Location"] == "/setup", (status, headers)
        status, headers, _ = h.http("POST", "/setup", {"password": "carotte-42", "confirm": "carotte-42"})
        assert "err=" in headers["Location"], "setup accepted without a button press"
        h.publish(f"{PREFIX}/core/event/button", json.dumps({"v": 1, "event": "click", "time": time.time()}))
        time.sleep(0.5)
        status, headers, _ = h.http("POST", "/setup", {"password": "carotte-42", "confirm": "carotte-42"})
        assert status == 303 and headers["Location"] == "/settings", headers
        cookie["v"] = headers["Set-Cookie"].split(";")[0]
        status, _, body = h.http("GET", "/", cookie=cookie["v"])
        assert status == 200 and "réveillé" in body, status

    def authenticated_action_reaches_core():
        status, _, _ = h.http("POST", "/action", {"name": "time"}, cookie=None)
        assert status == 401, "anonymous action"
        status, _, _ = h.http("POST", "/action", {"name": "time"}, cookie=cookie["v"], origin=False)
        assert status == 403, "cross-site action"
        t = time.time()
        status, headers, _ = h.http("POST", "/action", {"name": "play", "resource": "system/abort.wav"}, cookie=cookie["v"])
        assert status == 303 and "ok=" in headers["Location"], headers
        ok = [m for _, _, m in h.messages(f"{PREFIX}/core/result", t) if m["id"].startswith("svc-") and m["status"] == "ok"]
        assert ok, "no service command executed by the core"

    def settings_persist():
        form = {"locale": "en_US", "timezone": "Europe/Paris", "volume": "50", "wakeup": "00:00", "sleep": "00:00",
                "location": "", "unit": "celsius", "animation": "nothing", "frequency": "0", "ha_host": "",
                "ha_port": "1883", "ha_prefix": "homeassistant"}
        for i in range(7):
            form[f"wakeup_{i}"] = form[f"sleep_{i}"] = "00:00"
        status, headers, _ = h.http("POST", "/settings", form, cookie=cookie["v"])
        assert "ok=" in headers["Location"], headers
        saved = json.load(open(f"{h.tmp}/data/config.json"))
        assert saved["volume"] == 50 and saved["locale"] == "en_US" and saved["admin"]["hash"], saved
        settings = [m for _, _, m in h.messages(f"{PREFIX}/service/settings") if isinstance(m, dict)]
        assert settings and settings[-1]["locale"] == "en_US", settings

    def sleep_and_wakeup_from_ui():
        t = time.time()
        h.http("POST", "/action", {"name": "sleep"}, cookie=cookie["v"])
        h.state_since(t, "asleep", timeout=10)
        t = time.time()
        h.http("POST", "/action", {"name": "wakeup"}, cookie=cookie["v"])
        h.state_since(t, "idle", timeout=10)

    for name, fn in [("healthz, setup needs a button press", healthz_and_setup),
                     ("authenticated, same-origin UI action reaches the core", authenticated_action_reaches_core),
                     ("settings saved atomically and published", settings_persist),
                     ("sleep and wake up from the UI", sleep_and_wakeup_from_ui)]:
        h.check(name, fn)

    def broker_restart():
        h.command("before", "play", {"sequence": [{"audio": ["system/abort.wav"]}]})
        assert h.result("before")["status"] == "ok"
        h.stop("mosquitto")
        h.stop("sub")
        time.sleep(1)
        assert not h.publish(f"{PREFIX}/core/cmd", "{}"), "broker still up"
        assert h.http("GET", "/healthz")[0] == 503, "healthz must fail without broker"
        status, headers, _ = h.http("POST", "/action", {"name": "play", "resource": "system/abort.wav"}, cookie=cookie["v"])
        assert "err=" in headers["Location"], "UI must report the outage"
        t = time.time()
        h.start_broker()
        h.start_sub_reader()
        h.online_since(t)
        h.wait_for(lambda: h.http("GET", "/healthz")[0] == 200, 20, "healthz back")
        h.command("after", "play", {"sequence": [{"audio": ["system/abort.wav"]}]})
        assert h.result("after")["status"] == "ok"
        time.sleep(1.5)
        replayed = [m for _, _, m in h.messages(f"{PREFIX}/core/result", t) if m["id"] in ("before", "p1")]
        assert not replayed, f"commands replayed after reconnect: {replayed}"
        # The service resynchronises (ears, infos) after reconnecting; nothing
        # else may be played: the play refused during the outage stays dropped.
        played = {m["playing"] for _, _, m in h.messages(f"{PREFIX}/core/state", t) if m.get("playing")}
        assert played <= {"after"}, f"command replayed after reconnect: {played}"

    def core_restart_resync():
        t = time.time()
        h.stop("core", signal.SIGKILL)  # LWT path
        h.online_since(t, "offline")
        h.wait_for(lambda: h.http("GET", "/healthz")[0] == 503, 10, "healthz 503")
        t = time.time()
        h.start_core()
        h.online_since(t)
        h.wait_for(lambda: h.http("GET", "/healthz")[0] == 200, 15, "healthz 200")
        h.wait_for(lambda: [m for _, _, m in h.messages(f"{PREFIX}/core/result", t)
                            if m["id"].startswith("svc-")], 10, "service resync commands")

    for name, fn in [("broker restart: reconnect without replay", broker_restart),
                     ("core crash: LWT, restart and resync", core_restart_resync)]:
        h.check(name, fn)


if __name__ == "__main__":
    main()
