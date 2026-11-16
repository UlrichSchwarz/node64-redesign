-- IPAM64 0.7.0: fremde DHCP-Server (Router, Firewall) melden Netze, feste Adressen und Vergaben ueber die API.
-- Jede Meldung ersetzt den Stand ihrer Quelle vollstaendig. Je Node (nicht im Cluster abgeglichen, wie die
-- Sichtungen). Zeiten als Unix-Sekunden (UTC), 0 = unbekannt.

CREATE TABLE IF NOT EXISTS ipam_sources (
	name       TEXT PRIMARY KEY, -- Kennung in der Adresse, z. B. "pfsense"
	label      TEXT NOT NULL DEFAULT '', -- Anzeige, z. B. "pfSense"
	updated_at INTEGER NOT NULL
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS ipam_source_networks (
	source      TEXT NOT NULL,
	subnet      TEXT NOT NULL,
	name        TEXT NOT NULL DEFAULT '',
	iface       TEXT NOT NULL DEFAULT '',
	router      TEXT NOT NULL DEFAULT '',
	range_start TEXT NOT NULL DEFAULT '',
	range_end   TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (source, subnet)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS ipam_source_leases (
	source  TEXT NOT NULL,
	ip      TEXT NOT NULL,
	kind    TEXT NOT NULL, -- fixed (feste Adresse) | dhcp (Vergabe)
	mac     TEXT NOT NULL DEFAULT '',
	name    TEXT NOT NULL DEFAULT '',
	note    TEXT NOT NULL DEFAULT '',
	expires INTEGER NOT NULL DEFAULT 0,
	seen    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (source, ip, kind)
) WITHOUT ROWID;
