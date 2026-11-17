#!/usr/bin/env python3
"""node64-ops: meldet die geplanten Aufgaben dieses Hosts an Node64 (Modul OPS64).

Gelesen werden (nur lesend, nichts am Host wird geaendert):
  - Proxmox VE: Backup-Jobs (pvesh get /cluster/backup) und Backup-Laeufe (Aufgaben vom Typ vzdump)
  - Proxmox Backup Server: Sync-, Verify- und Prune-Jobs, Garbage Collection und ihre Laeufe
  - systemd-Timer mit dem letzten Lauf ihres Dienstes
  - crontabs (/etc/crontab, /etc/cron.d, crontabs der Benutzer)

Einstellungen in /etc/node64-ops.conf (Zeilen "name = wert") oder als Optionen:
  url      Adresse der Node, z. B. https://node64.example:8443
  key      API-Key der Node (node64_...)
  name     Kennung dieses Hosts (Standard: Hostname)
  label    Anzeigename (Standard: Hostname)
  insecure yes = Zertifikat der Node nicht pruefen (selbst signiert)
  skip     Bereiche auslassen, mit Komma: pve, pbs, timers, cron

Aufruf per cron, z. B. alle 5 Minuten:  */5 * * * * root /usr/local/bin/node64-ops
Python 3.7 oder neuer, keine Zusatzpakete.
"""

import argparse
import hashlib
import json
import os
import re
import shutil
import socket
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.request

VERSION = "1"
CONFIG = "/etc/node64-ops.conf"
MAX_JOB_RUNS = 50
MAX_RUNS = 300

# Nur fuer Tests: Verzeichnis mit root/ (Dateien) und cmd/ (Ausgaben von Befehlen) statt des echten Hosts.
FIXTURE = os.environ.get("NODE64_OPS_FIXTURE", "")

notes = []


def note(text):
    if text not in notes and len(notes) < 10:
        notes.append(text[:200])


def fx_name(cmd):
    return re.sub(r"[^A-Za-z0-9._-]+", "_", " ".join(cmd)).strip("_")[:120]


def have(tool):
    if FIXTURE:
        return any(f.startswith(tool) for f in os.listdir(os.path.join(FIXTURE, "cmd")))
    return shutil.which(tool) is not None


def run(cmd, timeout=60):
    """Fuehrt einen Befehl aus; liefert (ok, Ausgabe)."""
    if FIXTURE:
        p = os.path.join(FIXTURE, "cmd", fx_name(cmd))
        if not os.path.exists(p):
            return False, ""
        with open(p, encoding="utf-8") as f:
            return True, f.read()
    try:
        r = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout,
                           env=dict(os.environ, LC_ALL="C", LANG="C"))
    except (OSError, subprocess.SubprocessError) as e:
        return False, str(e)
    return r.returncode == 0, r.stdout.decode("utf-8", "replace")


def run_json(cmd):
    ok, out = run(cmd)
    if not ok:
        note("%s: failed" % " ".join(cmd[:3]))
        return None
    try:
        return json.loads(out)
    except ValueError:
        note("%s: no JSON" % " ".join(cmd[:3]))
        return None


def path(p):
    return os.path.join(FIXTURE, "root", p.lstrip("/")) if FIXTURE else p


def read(p):
    try:
        with open(path(p), encoding="utf-8", errors="replace") as f:
            return f.read()
    except OSError:
        return None


def listdir(p):
    try:
        return sorted(os.listdir(path(p)))
    except OSError:
        return []


def hostname():
    if FIXTURE:
        return (read("/etc/hostname") or "host").strip()
    return socket.gethostname().split(".")[0]


def timezone():
    tz = (read("/etc/timezone") or "").strip()
    if tz:
        return tz
    try:
        link = os.readlink(path("/etc/localtime"))
        if "zoneinfo/" in link:
            return link.split("zoneinfo/", 1)[1]
    except OSError:
        pass
    return ""


# ---- Geheimnisse in Befehlszeilen unkenntlich machen ----

MASKS = [
    (re.compile(r"(?i)((?:pass(?:word|wd)?|token|secret|api[-_]?key|apikey|auth|credentials?)\s*[=:]\s*)(\"[^\"]*\"|'[^']*'|\S+)"), r"\1***"),
    (re.compile(r"(?i)(--?(?:password|pass|token|secret|api-key|apikey|key|auth)[ =])(\"[^\"]*\"|'[^']*'|\S+)"), r"\1***"),
    (re.compile(r"(://[^/\s:@]+):[^@\s/]+@"), r"\1:***@"),
    (re.compile(r"(?i)(bearer\s+)\S+"), r"\1***"),
    (re.compile(r"\b(node64_)[A-Za-z0-9_-]{8,}"), r"\1***"),
]
MYSQL_PW = re.compile(r"(\s-p)\S+")


def mask(cmd):
    for rx, rep in MASKS:
        cmd = rx.sub(rep, cmd)
    if re.search(r"\b(mysql|mysqldump|mariadb|mariadb-dump)\b", cmd):
        cmd = MYSQL_PW.sub(r"\1***", cmd)
    return cmd[:300]


def short_id(*parts):
    return hashlib.sha1("|".join(parts).encode("utf-8")).hexdigest()[:12]


# Aufgaben aus Paketen gelten als "System" (in OPS64 standardmaessig ausgeblendet) - ausser sie sind fuer den Betrieb
# interessant.
INTERESTING = re.compile(r"zfs|scrub|trim|backup|vzdump|restic|borg|rsync|rclone|snapshot|sanoid|syncoid|certbot|acme|smart|raid|mdadm", re.I)


# ---- cron ----

CRON_ENV = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*\s*=")
WRAPPERS = {"nice", "ionice", "flock", "env", "/usr/bin/env", "sudo", "chronic", "timeout", "exec", "cd", "test", "[", "command", "systemd-cat"}
SHELL_WORDS = {"if", "then", "else", "fi", "do", "done"}
INTERPRETERS = {"sh", "bash", "dash", "zsh", "python", "python3", "perl", "php", "ruby", "node"}


def cron_name(cmd):
    """Kurzer Name aus dem Befehl: das Programm bzw. das Skript, das ein Interpreter ausfuehrt."""
    part = re.split(r"\s*(?:&&|\|\||;|\|)\s*", cmd)
    for seg in reversed(part):  # das letzte Stueck ist meist die eigentliche Arbeit ("test -x ... && run")
        toks = [t for t in seg.replace("{", " ").replace("}", " ").split()
                if not t.startswith("-") and not CRON_ENV.match(t) and t not in (">", ">>", "2>&1") and t not in SHELL_WORDS]
        while toks and (toks[0] in WRAPPERS or toks[0].startswith(">")):
            toks = toks[1:] if toks[0] not in ("cd", "test", "[") else []
        if not toks:
            continue
        name = os.path.basename(toks[0])
        if name in INTERPRETERS and len(toks) > 1:
            name = os.path.basename(toks[1])
        if name == "run-parts" and len(toks) > 1:
            name = "run-parts " + os.path.basename(toks[-1])
        if name:
            return name[:60]
    return cmd[:60]


def dpkg_owned(files):
    if not files or not have("dpkg-query"):
        return set()
    ok, out = run(["dpkg-query", "-S"] + files)
    owned = set()
    for line in out.splitlines():  # "paket: /etc/cron.d/datei" (auch bei Fehlern fuer einzelne Dateien)
        if ": " in line:
            owned.add(line.split(": ", 1)[1].strip())
    return owned


def cron_jobs():
    files = []  # (Datei, mit Benutzer-Feld, Benutzer)
    if read("/etc/crontab") is not None:
        files.append(("/etc/crontab", True, ""))
    crond = ["/etc/cron.d/" + f for f in listdir("/etc/cron.d") if "." not in f and not f.endswith("~")]
    files += [(f, True, "") for f in crond]
    for d in ("/var/spool/cron/crontabs", "/var/spool/cron"):
        for f in listdir(d):
            if os.path.isfile(path(d + "/" + f)):
                files.append((d + "/" + f, False, f))
    owned = dpkg_owned(crond)
    jobs = []
    for fname, has_user, user in files:
        text = read(fname)
        if text is None:
            if not has_user:
                note("crontab %s: not readable" % user)
            continue
        for line in text.splitlines():
            line = line.strip()
            if not line or line.startswith("#") or CRON_ENV.match(line):
                continue
            f = line.split(None, 1)
            if line.startswith("@"):
                sched, rest = f[0], (f[1] if len(f) > 1 else "")
            else:
                f = line.split(None, 5)
                if len(f) < 6:
                    continue
                sched, rest = " ".join(f[:5]), f[5]
            u = user
            if has_user:
                f = rest.split(None, 1)
                if len(f) < 2:
                    continue
                u, rest = f
            if "node64-ops" in rest:  # der Sammler selbst
                continue
            name = cron_name(rest)
            system = (fname == "/etc/crontab" and "run-parts" in rest) or fname in owned
            if system and INTERESTING.search(rest + " " + fname):
                system = False
            jobs.append({
                "id": "cron-" + short_id(fname, sched, u, rest), "name": name if u in ("root", "") else "%s (%s)" % (name, u),
                "kind": "cron", "schedule": sched, "syntax": "cron", "command": mask(rest), "system": system, "history": False,
            })
    return jobs


# ---- systemd-Timer ----

def show_blocks(units, props):
    """systemctl show fuer mehrere Units: Liste von dicts (mehrfache Eigenschaften als Liste unter name+"[]")."""
    if not units:
        return []
    base = ["systemctl", "show", "--no-pager"] + ["--property=" + ",".join(props)]
    ok, out = run(base + ["--timestamp=unix"] + units)
    if not ok:  # aeltere systemd-Versionen kennen --timestamp nicht
        ok, out = run(base + units)
        if not ok:
            note("systemctl show: failed")
            return []
    blocks = []
    for chunk in out.split("\n\n"):
        d = {}
        for line in chunk.splitlines():
            if "=" not in line:
                continue
            k, v = line.split("=", 1)
            d.setdefault(k + "[]", []).append(v)
            d[k] = v
        if d:
            blocks.append(d)
    return blocks


def stamp(v):
    """Zeitstempel aus systemctl show: "@1700000000" bzw. "Tue 2026-11-17 06:00:01 CET"; 0 = keiner."""
    v = (v or "").strip()
    if not v or v == "n/a" or v == "0":
        return 0
    if v.startswith("@"):
        try:
            return int(float(v[1:]))
        except ValueError:
            return 0
    m = re.search(r"(\d{4}-\d{2}-\d{2}) (\d{2}:\d{2}:\d{2})", v)
    if not m:
        return 0
    try:
        return int(time.mktime(time.strptime(m.group(1) + " " + m.group(2), "%Y-%m-%d %H:%M:%S")))
    except (ValueError, OverflowError):
        return 0


def timer_jobs():
    ok, out = run(["systemctl", "list-units", "--type=timer", "--all", "--no-legend", "--plain", "--no-pager"])
    if not ok:
        note("systemctl list-units: failed")
        return []
    names = [l.split()[0] for l in out.splitlines() if l.split() and l.split()[0].endswith(".timer")]
    timers = show_blocks(names, ["Id", "Unit", "Description", "TimersCalendar", "TimersMonotonic", "ActiveState", "FragmentPath"])
    services = {}
    for b in show_blocks(sorted({t.get("Unit", "") for t in timers if t.get("Unit")}),
                         ["Id", "ActiveState", "SubState", "Result", "ExecMainStatus", "ExecStart", "InactiveExitTimestamp", "StateChangeTimestamp"]):
        services[b.get("Id", "")] = b
    jobs = []
    for t in timers:
        tid = t.get("Id", "")
        if not tid or "node64-ops" in tid:
            continue
        specs = []
        for v in t.get("TimersCalendar[]", []):
            m = re.search(r"OnCalendar=(.*?) ; next_elapse", v)
            if m:
                specs.append(m.group(1).strip())
        frag = t.get("FragmentPath", "")
        system = frag.startswith(("/lib/systemd/", "/usr/lib/systemd/")) and not INTERESTING.search(tid)
        job = {"id": "timer-" + tid, "name": tid[:-len(".timer")], "kind": "timer", "schedule": "; ".join(specs), "syntax": "calendar",
               "enabled": t.get("ActiveState") == "active", "system": system, "history": False}
        if t.get("Description") and t["Description"] != tid:
            job["target"] = t["Description"][:120]
        s = services.get(t.get("Unit", ""))
        if s:
            job["history"] = True
            m = re.search(r"argv\[\]=(.*?) ; ignore_errors", s.get("ExecStart", ""))
            if m:
                job["command"] = mask(m.group(1))
            start = stamp(s.get("InactiveExitTimestamp"))
            if start:
                state, sub = s.get("ActiveState", ""), s.get("SubState", "")
                if state in ("activating", "deactivating") or (state == "active" and sub != "exited"):
                    job["runs"] = [{"start": start, "end": 0}]
                else:
                    end = max(stamp(s.get("StateChangeTimestamp")), start)
                    good = s.get("Result", "success") == "success" and s.get("ExecMainStatus", "0") in ("0", "")
                    r = {"start": start, "end": end, "status": "ok" if good else "fail"}
                    if not good:
                        r["detail"] = "Result=%s, exit %s" % (s.get("Result", "?"), s.get("ExecMainStatus", "?"))
                    job["runs"] = [r]
        jobs.append(job)
    return jobs


# ---- Proxmox VE ----

def pve_jobs():
    node = hostname()
    jobs, runs = [], []
    lst = run_json(["pvesh", "get", "/cluster/backup", "--output-format", "json"])
    for j in lst or []:
        if j.get("node") and j["node"] != node:  # Job eines anderen Knotens im Cluster
            continue
        jid = str(j.get("id") or "")
        if not jid:
            continue
        sched = j.get("schedule") or ""
        if not sched and j.get("starttime"):  # alte Schreibweise: Wochentage und Uhrzeit getrennt
            sched = ("%s %s" % (j.get("dow", ""), j["starttime"])).strip()
        if j.get("all"):
            sel = "--all"
        elif j.get("pool"):
            sel = "--pool %s" % j["pool"]
        else:
            sel = str(j.get("vmid", ""))
        enabled = j.get("enabled", 1) not in (0, "0", False)
        jobs.append({
            "id": "vzdump-" + jid, "name": (j.get("comment") or "vzdump " + sel)[:120], "kind": "backup", "target": str(j.get("storage", "")),
            "schedule": sched, "syntax": "calendar", "command": mask("vzdump %s --storage %s --mode %s" % (sel, j.get("storage", ""), j.get("mode", "snapshot"))),
            "enabled": enabled, "history": True,
        })
    tasks = run_json(["pvesh", "get", "/nodes/%s/tasks" % node, "--typefilter", "vzdump", "--source", "all", "--limit", "200", "--output-format", "json"])
    for t in tasks or []:
        start = int(t.get("starttime") or 0)
        if not start:
            continue
        end = int(t.get("endtime") or 0)
        status = str(t.get("status") or "")
        r = {"kind": "backup", "name": ("vzdump %s" % t["id"]) if t.get("id") else "vzdump", "start": start, "end": end, "ref": str(t.get("upid", ""))[:200]}
        if end:
            good = status == "OK" or status.upper().startswith("WARNINGS")
            r["status"] = "ok" if good else "fail"
            if status != "OK":
                r["detail"] = status[:300]
        runs.append(r)
    return jobs, runs


# ---- Proxmox Backup Server ----

PBS_TYPES = {"syncjob": "sync", "verificationjob": "verify", "prunejob": "prune", "garbage_collection": "gc"}


def pbs_jobs():
    jobs = {}
    pbm = "proxmox-backup-manager"
    for sub, label in (("sync-job", "sync"), ("verify-job", "verify"), ("prune-job", "prune")):
        for j in run_json([pbm, sub, "list", "--output-format", "json"]) or []:
            jid = str(j.get("id") or "")
            if not jid:
                continue
            target = str(j.get("store", ""))
            if label == "sync":
                target = "%s:%s → %s" % (j.get("remote", "local"), j.get("remote-store", ""), j.get("store", ""))
            jobs[(label, jid)] = {"id": "pbs-%s-%s" % (label, jid), "name": (j.get("comment") or "%s %s" % (label, jid))[:120], "kind": "backup",
                                  "target": target[:120], "schedule": j.get("schedule") or "", "syntax": "calendar", "history": True, "runs": []}
    for s in run_json([pbm, "datastore", "list", "--output-format", "json"]) or []:
        name = str(s.get("name") or "")
        if name and s.get("gc-schedule"):
            jobs[("gc", name)] = {"id": "pbs-gc-" + name, "name": "garbage collection " + name, "kind": "backup", "target": name,
                                  "schedule": s["gc-schedule"], "syntax": "calendar", "history": True, "runs": []}
    for t in run_json([pbm, "task", "list", "--all", "--limit", "300", "--output-format", "json"]) or []:
        label = PBS_TYPES.get(str(t.get("worker_type", "")))
        start = int(t.get("starttime") or 0)
        if not label or not start:
            continue
        wid = str(t.get("worker_id") or "")
        job = jobs.get((label, wid)) or jobs.get((label, wid.split(":")[-1]))
        if job is None or len(job["runs"]) >= MAX_JOB_RUNS:
            continue
        end = int(t.get("endtime") or 0)
        status = str(t.get("status") or "")
        r = {"start": start, "end": end, "ref": str(t.get("upid", ""))[:200]}
        if end:
            good = status == "OK" or status.upper().startswith("WARNINGS")
            r["status"] = "ok" if good else "fail"
            if status != "OK":
                r["detail"] = status[:300]
        job["runs"].append(r)
    return list(jobs.values())


# ---- Meldung bauen und senden ----

def collect(skip):
    jobs, runs = [], []
    if "pve" not in skip and have("pvesh"):
        j, r = pve_jobs()
        jobs += j
        runs += r
    if "pbs" not in skip and have("proxmox-backup-manager"):
        jobs += pbs_jobs()
    if "timers" not in skip and have("systemctl"):
        jobs += timer_jobs()
    if "cron" not in skip:
        jobs += cron_jobs()
    seen, out = set(), []
    for j in jobs:  # Kennungen muessen eindeutig sein
        if j["id"] in seen:
            continue
        seen.add(j["id"])
        j["id"] = j["id"][:120]
        if "runs" in j:
            j["runs"] = sorted(j["runs"], key=lambda r: -r["start"])[:MAX_JOB_RUNS]
        out.append(j)
    runs = sorted(runs, key=lambda r: -r["start"])[:MAX_RUNS]
    return out[:500], runs


def load_config(p):
    cfg = {}
    try:
        with open(p, encoding="utf-8") as f:
            text = f.read()
    except OSError:
        return cfg
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, v = line.split("=", 1)
        cfg[k.strip().lower()] = v.strip().strip('"').strip("'")
    return cfg


def main():
    ap = argparse.ArgumentParser(description="Meldet Backup-Jobs, Cronjobs und Timer dieses Hosts an Node64 (OPS64).")
    ap.add_argument("--config", default=CONFIG, help="Datei mit den Einstellungen (Standard %s)" % CONFIG)
    ap.add_argument("--url", help="Adresse der Node, z. B. https://node64.example:8443")
    ap.add_argument("--key", help="API-Key der Node (besser in der Datei mit den Einstellungen)")
    ap.add_argument("--name", help="Kennung dieses Hosts (Standard: Hostname)")
    ap.add_argument("--label", help="Anzeigename (Standard: Hostname)")
    ap.add_argument("--skip", help="Bereiche auslassen: pve,pbs,timers,cron")
    ap.add_argument("--insecure", action="store_true", help="Zertifikat der Node nicht pruefen")
    ap.add_argument("--dry-run", action="store_true", help="nur zeigen, was gemeldet wuerde")
    ap.add_argument("--version", action="version", version="node64-ops " + VERSION)
    a = ap.parse_args()
    cfg = load_config(a.config)
    url = (a.url or cfg.get("url", "")).rstrip("/")
    key = a.key or cfg.get("key", "")
    host = hostname()
    name = re.sub(r"[^a-z0-9._-]+", "-", (a.name or cfg.get("name") or host).lower()).strip("-.")[:63] or "host"
    skip = {s.strip() for s in (a.skip or cfg.get("skip", "")).split(",") if s.strip()}
    insecure = a.insecure or cfg.get("insecure", "").lower() in ("1", "yes", "true", "on")

    jobs, runs = collect(skip)
    body = {"label": a.label or cfg.get("label") or host, "collector": "linux " + VERSION, "timezone": timezone(), "notes": notes, "jobs": jobs, "runs": runs}
    if a.dry_run:
        json.dump(body, sys.stdout, indent=2, ensure_ascii=False, sort_keys=True)
        sys.stdout.write("\n")
        sys.stderr.write("node64-ops: %d jobs, %d runs without job, name %s (not sent)\n" % (len(jobs), len(runs), name))
        return 0
    if not url or not key:
        sys.stderr.write("node64-ops: url and key are required (see %s or --help)\n" % a.config)
        return 2
    req = urllib.request.Request(url + "/api/ops64/sources/" + name, data=json.dumps(body).encode("utf-8"), method="PUT",
                                 headers={"Authorization": "Bearer " + key, "Content-Type": "application/json", "User-Agent": "node64-ops/" + VERSION})
    ctx = None
    if url.startswith("https://"):
        ctx = ssl.create_default_context()
        if insecure:
            ctx.check_hostname = False
            ctx.verify_mode = ssl.CERT_NONE
    try:
        with urllib.request.urlopen(req, timeout=30, context=ctx) as resp:
            resp.read()
    except urllib.error.HTTPError as e:
        hint = {401: "API key not accepted", 403: "API key is read-only", 404: "OPS64 is switched off on the node (or wrong address)"}.get(e.code, "")
        sys.stderr.write("node64-ops: node answered %d %s\n" % (e.code, hint))
        return 1
    except (urllib.error.URLError, OSError) as e:
        sys.stderr.write("node64-ops: cannot reach %s: %s\n" % (url, getattr(e, "reason", e)))
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
