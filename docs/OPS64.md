# OPS64 – Betrieb & Wartung

OPS64 ist das Node64-Modul für geplante Aufgaben im Heimnetz: Backups, Cronjobs und Timer aller Hosts in einem
Zeitplan – mit Zuständen, Überschneidungen und Meldungen. Entwurf und Umsetzung kommen aus der Community
(UlrichSchwarz).

OPS64 **liest nur mit**. Es startet, stoppt und sperrt nichts auf den Hosts.

## Einschalten und Hosts einbinden

1. *Einstellungen → Module → OPS64* einschalten (Standard aus; aus heißt: kein Menüpunkt, keine Hintergrundarbeit,
   Meldungen der Hosts werden mit 404 abgewiesen).
2. Unter *API* einen API-Key anlegen.
3. Auf jedem Host den Sammler einrichten (*OPS64 → Host einbinden* zeigt die Befehle mit der Adresse der Node):

   ```bash
   curl -fsS -H "Authorization: Bearer $KEY" -o /usr/local/bin/node64-ops https://NODE/api/ops64/collector/linux.py
   chmod 755 /usr/local/bin/node64-ops
   printf 'url = https://NODE\nkey = %s\n' "$KEY" > /etc/node64-ops.conf && chmod 600 /etc/node64-ops.conf
   node64-ops --dry-run     # zeigt, was gemeldet würde
   node64-ops               # meldet einmal
   echo '*/5 * * * * root /usr/local/bin/node64-ops' > /etc/cron.d/node64-ops
   ```

Der Sammler braucht Python 3.7 oder neuer und keine Zusatzpakete. Einstellungen in `/etc/node64-ops.conf`:
`url`, `key`, `name` (Kennung, Standard Hostname), `label` (Anzeigename), `insecure = yes` (selbst signiertes
Zertifikat der Node), `skip = pve,pbs,timers,cron` (Bereiche auslassen).

## Was der Sammler liest

| Bereich | Quelle | Läufe |
|---|---|---|
| Backup-Jobs Proxmox VE | `pvesh get /cluster/backup` | Aufgaben vom Typ `vzdump` des Knotens (auch Backups von Hand) |
| Proxmox Backup Server | `proxmox-backup-manager sync-job/verify-job/prune-job list`, Garbage Collection der Datastores | `proxmox-backup-manager task list` |
| systemd-Timer | `systemctl list-units --type=timer`, `systemctl show` | der letzte Lauf des Dienstes |
| Cronjobs | `/etc/crontab`, `/etc/cron.d/*`, crontabs der Benutzer | keine (cron führt kein Protokoll) |

- Passwörter, Token und Schlüssel in Befehlszeilen werden vor dem Senden durch `***` ersetzt.
- Aufgaben aus Paketen (z. B. `apt-daily`, `cron.daily`) gelten als Aufgaben des Systems und sind im Zeitplan
  standardmäßig ausgeblendet; Scrub, Trim, Backup und Ähnliches bleiben sichtbar.
- Kann der Sammler etwas nicht lesen, steht ein Hinweis beim Host unter *Hosts*.
- Im Proxmox-Cluster läuft der Sammler auf jedem Knoten; Jobs, die auf einen anderen Knoten beschränkt sind,
  meldet er nicht.

Der Sammler für Proxmox VE und Proxmox Backup Server ist gegen nachgestellte Ausgaben getestet
(`internal/ops64/testdata/linux`), noch nicht gegen viele echte Installationen – Rückmeldungen willkommen.

## Zeitplan

- **Tag, Woche, Monat.** Je Aufgabe eine Zeile: Balken auf der Zeitachse des gewählten Tages (*Nacht und Vormittag*
  oder *Ganzer Tag*) und in der Wochenansicht der Zustand je Wochentag. Ein Klick auf einen Wochentag zeigt dessen
  Tagesablauf, ein Klick auf eine Aufgabe ihre Einzelheiten und den Verlauf.
- **Zeitpläne** liest die Node selbst, in der Zeitzone des Hosts: cron mit fünf Feldern und `@daily`, `@weekly` …;
  Kalender-Angaben von systemd und Proxmox (`mon..fri 02:00`, `sat 03:30`, `*-*-01 04:00:00`, `2,22:30`, `*/30`,
  `daily`). Nicht lesbar sind feste Jahre, `~` (letzte Tage des Monats) und Angaben mit Zeitzone; solche Aufgaben
  stehen in der Liste, aber nicht im Zeitplan. Timer ohne festen Zeitpunkt (z. B. `OnUnitActiveSec`) ebenso.
- **Zustände:** geplant, laufend, erfolgreich, fehlgeschlagen, überfällig, ohne Rückmeldung. *Überfällig* gibt es
  nur für Aufgaben, deren Läufe der Host meldet, und nur, wenn der Host sich nach dem Zeitpunkt gemeldet hat
  (10 Minuten Karenz). Cronjobs haben deshalb nur *geplant* und *ohne Rückmeldung*.
- **Erwartete Dauer:** von Hand, sonst der Median der letzten zehn erfolgreichen Läufe (schwanken sie stark, der
  längste), sonst die Angabe des Hosts. Ohne Angabe: *Laufzeit unklar* (im Zeitplan mit 30 Minuten gezeichnet).
- **Gleichzeitig:** Laufen bzw. liegen Aufgaben verschiedener Hosts oder desselben Hosts zur selben Zeit, zeigt
  der Zeitplan ein rotes Band und nennt die Aufgaben. Überfällige Einträge zählen nicht.
- Aufgaben, die öfter als 24-mal am Tag laufen, stehen nicht einzeln im Zeitplan.
- Läufe, die zu keinem Zeitplan passen (Backup von Hand), erscheinen als eigene Zeile „außer der Reihe“.

## Meldungen

Über die Standardkanäle (*Benachrichtigungen*), je Art abschaltbar, jede Meldung einmal:

- **Fehler:** ein Lauf ist fehlgeschlagen.
- **Überfällig:** eine Aufgabe ist 10 Minuten nach ihrem Zeitpunkt nicht gestartet.
- **Lange Laufzeit:** ein Lauf dauert mehr als doppelt so lange wie erwartet (mindestens 15 Minuten länger).
- **Überschneidung:** Aufgaben sind in den nächsten 8 Tagen gleichzeitig geplant (je Gruppe einmal).
- **Host stumm:** ein Host hat sich eine Stunde nicht gemeldet.

Nach dem Einbinden eines Hosts wird nur verschickt, was höchstens 2 Stunden alt ist.

## API

`/api/ops64/…` (Beschreibung unter *API* in der Node):

- `PUT /api/ops64/sources/{name}` – Meldung eines Hosts (alle Aufgaben, bekannte Läufe). Eigene Sammler für andere
  Systeme können dieselbe Schnittstelle nutzen.
- `GET /api/ops64/sources`, `DELETE /api/ops64/sources/{name}`
- `GET /api/ops64/jobs` – Aufgaben mit letztem und nächstem Lauf
- `GET /api/ops64/plan?from=…&to=…` – Einträge des Zeitplans und Überschneidungen

Läufe werden 90 Tage aufbewahrt. Die Daten bleiben je Node (kein Cluster-Abgleich in 0.1.0).

## Noch nicht enthalten

- **Keine gleichzeitigen Jobs (gemeinsame Steuerung):** Jeder eingebundene Job wartet, bis der vorige fertig ist.
  Dafür müsste OPS64 auf den Hosts eingreifen; 0.1.0 zeigt und meldet Überschneidungen nur.
- Hinweise auf LXC-Updates und Firmware.
