-- IPAM64 0.7.0: Ereignisse der fremden DHCP-Server (neue Vergabe, Vergabe beendet, anderes Geraet, feste Zuweisung,
-- Netz). Entstehen aus dem Vergleich zweier Meldungen einer Quelle. Je Node, 90 Tage bzw. hoechstens 5000 Zeilen.

CREATE TABLE IF NOT EXISTS ipam_events (
	id     INTEGER PRIMARY KEY,
	ts     INTEGER NOT NULL, -- Unix-Sekunden (UTC)
	source TEXT NOT NULL,    -- Kennung der Quelle
	label  TEXT NOT NULL DEFAULT '', -- Anzeige der Quelle zum Zeitpunkt des Ereignisses
	kind   TEXT NOT NULL,
	ip     TEXT NOT NULL DEFAULT '', -- Adresse bzw. Netz
	mac    TEXT NOT NULL DEFAULT '',
	name   TEXT NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT ''  -- z. B. die vorige MAC
);

CREATE INDEX IF NOT EXISTS ipam_events_ts ON ipam_events (ts);
