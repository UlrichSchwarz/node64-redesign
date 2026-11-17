-- OPS64: Betrieb und Wartung. Hosts melden ihre Aufgaben (Backup-Jobs, Cronjobs, Timer) und deren Laeufe ueber die
-- API (Sammler); die Node rechnet daraus den Zeitplan. Zeiten in Unix-Sekunden.
CREATE TABLE ops_sources (
	name      TEXT PRIMARY KEY,           -- Kennung aus der URL (a-z, 0-9, Punkt, Strich, Unterstrich)
	label     TEXT NOT NULL DEFAULT '',   -- Anzeigename des Hosts
	collector TEXT NOT NULL DEFAULT '',   -- Sammler und Version, z. B. "linux 1"
	timezone  TEXT NOT NULL DEFAULT '',   -- Zeitzone des Hosts (IANA), leer = die der Node
	notes     TEXT NOT NULL DEFAULT '',   -- Hinweise des Sammlers (JSON-Liste)
	first     INTEGER NOT NULL,           -- erste Meldung
	updated   INTEGER NOT NULL            -- letzte Meldung
);

CREATE TABLE ops_jobs (
	source   TEXT NOT NULL,
	id       TEXT NOT NULL,               -- Kennung der Aufgabe in der Quelle
	name     TEXT NOT NULL,
	kind     TEXT NOT NULL,               -- backup | cron | timer
	target   TEXT NOT NULL DEFAULT '',    -- Ziel (z. B. Speicher), nur Anzeige
	schedule TEXT NOT NULL DEFAULT '',    -- Zeitplan wie in der Quelle
	syntax   TEXT NOT NULL DEFAULT '',    -- cron | calendar (systemd, Proxmox), leer = erkennen
	command  TEXT NOT NULL DEFAULT '',    -- Befehl (Geheimnisse maskiert der Sammler)
	enabled  INTEGER NOT NULL DEFAULT 1,
	system   INTEGER NOT NULL DEFAULT 0,  -- Aufgabe des Systems (standardmaessig nicht im Zeitplan)
	history  INTEGER NOT NULL DEFAULT 0,  -- die Quelle meldet Laeufe (nur dann gibt es "ueberfaellig")
	expected INTEGER NOT NULL DEFAULT 0,  -- erwartete Dauer in Sekunden laut Quelle (0 = unbekannt)
	first    INTEGER NOT NULL DEFAULT 0,  -- zum ersten Mal gemeldet (davor gibt es kein "ueberfaellig")
	PRIMARY KEY (source, id)
);

-- Laeufe werden je Meldung ergaenzt (nicht ersetzt) und 90 Tage aufbewahrt. job leer = keinem Zeitplan zugeordnet
-- (z. B. Backup von Hand); name traegt dann die Bezeichnung.
CREATE TABLE ops_runs (
	source TEXT NOT NULL,
	job    TEXT NOT NULL DEFAULT '',
	kind   TEXT NOT NULL DEFAULT '',
	name   TEXT NOT NULL DEFAULT '',
	start  INTEGER NOT NULL,
	finish INTEGER NOT NULL DEFAULT 0,    -- Ende, 0 = laeuft noch
	status TEXT NOT NULL,                 -- ok | fail | running
	detail TEXT NOT NULL DEFAULT '',
	ref    TEXT NOT NULL DEFAULT '',      -- Kennung des Laufs in der Quelle (z. B. UPID), leer = start
	PRIMARY KEY (source, job, start, ref)
);
CREATE INDEX ops_runs_start ON ops_runs (start);

-- Einstellungen der Node je Aufgabe (bleiben, auch wenn die Aufgabe kurz nicht gemeldet wird).
CREATE TABLE ops_prefs (
	source   TEXT NOT NULL,
	job      TEXT NOT NULL,
	visible  TEXT NOT NULL DEFAULT '',    -- '' = Standard (System-Aufgaben aus), show | hide
	expected INTEGER NOT NULL DEFAULT 0,  -- erwartete Dauer von Hand in Sekunden (0 = aus den Laeufen)
	PRIMARY KEY (source, job)
);

-- Meldungen: Gedaechtnis (jede nur einmal) und Liste auf der Seite.
CREATE TABLE ops_alerts (
	key    TEXT PRIMARY KEY,
	kind   TEXT NOT NULL,                 -- fail | overdue | long | overlap | stale
	at     INTEGER NOT NULL,
	source TEXT NOT NULL DEFAULT '',
	job    TEXT NOT NULL DEFAULT '',
	name   TEXT NOT NULL DEFAULT '',
	data   TEXT NOT NULL DEFAULT '',      -- JSON (Platzhalter des Textes)
	open   INTEGER NOT NULL DEFAULT 1     -- Zustand besteht noch (overlap, stale)
);
