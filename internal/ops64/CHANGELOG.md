# OPS64 – Änderungen

## 0.1.0 – unveröffentlicht

Neu (Entwurf und Umsetzung aus der Community, UlrichSchwarz):

- Betrieb & Wartung an einer Stelle: Hosts melden ihre Backup-Jobs, Cronjobs und Timer samt Läufen an die Node (`PUT /api/ops64/sources/<name>`). OPS64 liest nur mit und ändert nichts an den Hosts.
- Sammler für Linux-Hosts (Python 3, ohne Zusatzpakete, per cron): Backup-Jobs und Backup-Läufe von Proxmox VE, Jobs des Proxmox Backup Server, systemd-Timer mit dem letzten Lauf und alle crontabs. Passwörter und Token in Befehlszeilen werden vor dem Senden unkenntlich gemacht. Anleitung und Download unter *OPS64 → Host einbinden*.
- Zeitplan für Tag, Woche und Monat: je Aufgabe eine Zeile mit Balken auf der Zeitachse (Nacht und Vormittag oder ganzer Tag) und dem Zustand je Wochentag; Zustände geplant, laufend, erfolgreich, fehlgeschlagen, überfällig, ohne Rückmeldung. Filter nach Host, Typ und Status.
- Die Node liest die Zeitpläne selbst: cron (auch `@daily` …) und Kalender-Angaben von systemd und Proxmox (`mon..fri 02:00`, `*-*-01 04:00:00`, `daily`), in der Zeitzone des Hosts.
- Erwartete Dauer aus den letzten erfolgreichen Läufen (Median; schwanken sie stark, der längste) oder von Hand; ohne Angabe „Laufzeit unklar“.
- Gleichzeitige Aufgaben: Überschneidungen über alle Hosts als rotes Band im Zeitplan, mit den beteiligten Aufgaben.
- Einzelheiten und Verlauf je Aufgabe; Aufgaben lassen sich aus- und einblenden (Aufgaben des Systems sind standardmäßig ausgeblendet).
- Listen „Backups“ und „Cronjobs & Timer“ mit letztem und nächstem Lauf.
- Meldungen über die Standardkanäle, je Art abschaltbar: Fehler, überfällige Aufgabe, ungewöhnlich langer Lauf, gleichzeitig geplante Aufgaben, Host meldet sich nicht mehr.
- API `/api/ops64/…` (Hosts, Aufgaben, Zeitplan), Dashboard-Karte, Audit-Bereich „OPS64“.

Noch nicht enthalten: gemeinsame Steuerung („keine gleichzeitigen Jobs“ – jeder Job wartet auf den vorigen), Hinweise auf LXC-Updates und Firmware.
