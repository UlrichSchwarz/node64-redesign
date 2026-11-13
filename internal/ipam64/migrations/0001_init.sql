-- Modul IPAM64: eigene Netze, von Hand eingetragene Adressen und Sichtungen aus der Nachbartabelle.
-- Alles andere (DHCP, lokaler DNS, Healthchecks, Monitor64, PROXY64) liest das Modul zur Laufzeit aus den Modulen.
-- Zeiten als Unix-Sekunden (UTC).

CREATE TABLE IF NOT EXISTS ipam_networks (
	id          INTEGER PRIMARY KEY,
	subnet      TEXT NOT NULL UNIQUE, -- "192.168.1.0/24" (Netzadresse)
	name        TEXT NOT NULL DEFAULT '',
	vlan        INTEGER NOT NULL DEFAULT 0, -- 0 = keins bzw. aus der Schnittstelle (eth0.20)
	iface       TEXT NOT NULL DEFAULT '',
	router      TEXT NOT NULL DEFAULT '',
	range_start TEXT NOT NULL DEFAULT '', -- DHCP-Bereich eines fremden DHCP-Servers (nur Anzeige)
	range_end   TEXT NOT NULL DEFAULT '',
	comment     TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS ipam_addresses (
	id         INTEGER PRIMARY KEY,
	ip         TEXT NOT NULL UNIQUE,
	mac        TEXT NOT NULL DEFAULT '', -- aa:bb:cc:dd:ee:ff, leer = unbekannt
	name       TEXT NOT NULL DEFAULT '',
	comment    TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);

-- je IP und MAC eine Zeile: wann zuerst und zuletzt erreichbar in der Nachbartabelle (ARP)
CREATE TABLE IF NOT EXISTS ipam_seen (
	ip         TEXT NOT NULL,
	mac        TEXT NOT NULL,
	first_seen INTEGER NOT NULL,
	last_seen  INTEGER NOT NULL,
	PRIMARY KEY (ip, mac)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS ipam_seen_last ON ipam_seen (last_seen);
