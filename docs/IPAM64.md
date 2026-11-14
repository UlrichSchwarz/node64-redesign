# IPAM64 – Integration in Node64

IPAM64 ist ein neues, optionales Modul (Entwurf aus der Community): die IP-Belegung der Netze im Heimnetz auf einer
Seite. Es pflegt kaum eigene Daten, sondern führt zusammen, was die Node schon weiß. Diese Anleitung richtet sich an
den Entwickler von Node64 und beschreibt, wie das Modul übernommen wird, was es am Bestand ändert und was vor einem
Release noch zu entscheiden oder zu prüfen ist.

Stand: Modulversion 0.1.0, entwickelt auf Node64 1.5.9 (Commit `8e33cc8`).

## Auf einen Blick

| | |
|---|---|
| Paket | `internal/ipam64` (Name `ipam64`, Pfad `/ipam64`, Menü nach DNS64, `Order: 45`) |
| Standard | aus – ausgeschaltet kein Menüpunkt, keine Hintergrundarbeit, kein Netz-Scan, alle Routen 404 |
| Umfang | nur IPv4, Netze von /20 bis /30 |
| Abhängigkeiten | keine neuen Go-Module; nur Standardbibliothek und `internal/module`, `internal/store` |
| Rechte | keine zusätzlichen: Netlink `RTM_GETNEIGH` lesen und UDP senden gehen ohne Capabilities |
| Ports | keine (das Modul lauscht nirgends, die Port-Verwaltung ist nicht berührt) |
| Datenbank | Scope `ipam64`, drei Tabellen `ipam_*`, eine Migration |
| Eingriff in den Bestand | zwei Zeilen in `main.go`, je eine Lesefunktion in DNS64 und PROXY64, sonst nur Ergänzungen |
| Portal, hubs, Edge | nicht berührt; kein Sync, keine neuen Check-Parameter |

## 1. Übernehmen

Die Arbeit liegt im Repository `UlrichSchwarz/node64-redesign` auf dem Zweig `claude/inspiring-hamilton-dpu6g9`,
direkt auf Release 1.5.9. Es sind zwei Commits: das Modul und diese Anleitung (mit dem Eintrag für die
Backup-Tests, siehe 5.3).

```bash
git remote add redesign https://github.com/UlrichSchwarz/node64-redesign.git
git fetch redesign claude/inspiring-hamilton-dpu6g9
git log --oneline 8e33cc8..redesign/claude/inspiring-hamilton-dpu6g9
git cherry-pick 8e33cc8..redesign/claude/inspiring-hamilton-dpu6g9
```

Ist der Hauptstand inzwischen weiter als 1.5.9, sind Konflikte nur in den Dateien zu erwarten, an die viele
Änderungen etwas anhängen. In allen Fällen sind beide Seiten zu behalten:

| Datei | Was IPAM64 dort tut |
|---|---|
| `CHANGELOG.md`, `CHANGELOG.en.md` | ein Stichpunkt unter „Unveröffentlicht“ / „Unreleased“ |
| `internal/i18n/locales/de.json`, `en.json` | `nav.ipam64`, `module.ipam64`, `modinfo.ipam64`, `audit.area.ipam64`, sechs `audit.actions.ipam64_*`, Block `ipam64` am Dateiende |
| `internal/web/static/openapi.json` | Tag `IPAM64` und drei Pfade vor `/sonar64/status` |
| `internal/web/static/css/node64.css` | Block „IPAM64“ am Dateiende |
| `README.md`, `docs/MODUL-VERSIONEN.md` | Abschnitt bzw. Tabellenzeile |

Die Sprachdateien sind von Hand ergänzt, nicht neu geschrieben: bestehende Zeilen sind unverändert.

## 2. Was sich am Bestand ändert

Neu sind `internal/ipam64/` (das ganze Modul) und `cmd/node64/ipam.go` (die Verdrahtung). Geändert:

| Datei | Änderung |
|---|---|
| `cmd/node64/main.go` | `modules.Register(newIPAM(...))` nach der Verdrahtung von Monitor64 |
| `internal/dns64/netdevices.go` | neue Lesefunktion `DHCPLeases()` – Vergaben des laufenden DHCP-Servers |
| `internal/cdn64/hosts.go` | neue Lesefunktion `ProxyTargets(ctx)` – Name und Ziel der eingeschalteten Proxy-Hosts |
| `internal/web/audit_areas.go` | Bereich `ipam64` für den Filter im Audit-Log |
| `internal/store/storetest/storetest.go` | Scope `ipam64` in `Scopes` (Backup- und Reset-Tests) |

DNS64 und PROXY64 ändern ihr Verhalten nicht, deshalb haben sie keine neue Modulversion bekommen. Healthchecks und
Monitor64 sind unverändert: `Store().Checks` und `Views()` gab es schon.

## 3. Verdrahtung

Das Modul kennt die anderen Module nicht. `cmd/node64/ipam.go` hängt die Quellen als Funktionen an, so wie
`SetNetDevices` bei den Healthchecks. Jede Quelle liefert nichts, solange ihr Modul nicht läuft
(`modules.IsRunning`).

```go
m := ipam64.New(st, log)
m.SetNetworks(func(ctx context.Context) []ipam64.NetHint { ... })            // DHCP-Netze aus DNS64
m.SetSource(ipam64.SrcDHCP, func(ctx context.Context) []ipam64.Sighting { ... })
```

| Quelle | Schlüssel | Woher | Liefert |
|---|---|---|---|
| DHCP | `dhcp` | `dns64.DHCPScopes`, `DHCPReservations`, `DHCPLeases` | Netze; feste Adressen (Art `fixed`); aktive Vergaben (Art `dhcp`) mit MAC und Hostname |
| Lokaler DNS | `dns` | `dns64.Records` | eingeschaltete A-Einträge ohne Platzhalter (`*`) |
| Monitor64 | `monitor64` | `monitor64.Views` | Adresse und Name der Hosts; „gesehen“, wenn ein Zugang OK ist |
| Healthchecks | `healthchecks` | `hc.Store().Checks` | Ziel und Name der hier gemessenen Checks; „gesehen“, wenn der Check `up` ist |
| PROXY64 | `proxy` | `cdn64.ProxyTargets` | Ziel der eingeschalteten Proxy-Hosts |

Dazu kommen drei Quellen im Modul selbst: von Hand eingetragene Adressen (`ipam`), die Adressen der Node (`node`)
und die Nachbartabelle (`scan`).

Die Reihenfolge der `SetSource`-Aufrufe bestimmt, wessen Name gewinnt: von Hand eingetragen vor DHCP, lokalem DNS,
Monitor64, Healthchecks, PROXY64. „Zuletzt gesehen“ ist das Maximum über alle Quellen. Bei einer DHCP-Vergabe ist es
die letzte Verlängerung (Ablauf minus Laufzeit des Netzes).

Ziele mit Namen statt IP (Healthchecks, PROXY64) werden über die bekannten Namen einer Adresse zugeordnet – der
volle Name oder sein erster Teil (`nas.home.lan` → `nas`). Es wird nichts im DNS aufgelöst; Ziele außerhalb der
bekannten Netze fallen weg.

### Eine weitere Quelle anhängen

```go
m.SetSource("meinmodul", func(ctx context.Context) []ipam64.Sighting {
	if !modules.IsRunning(meinmodul.Name) {
		return nil
	}
	return []ipam64.Sighting{{Addr: "192.168.1.50", Name: "geraet", Note: "Zusatz", Seen: zuletzt}}
})
```

`Addr` darf eine IP, ein Name, `host:port` oder eine URL sein. Für die Spalte „Bekannt aus“ braucht der Schlüssel
einen Text unter `ipam64.src.<schlüssel>` in beiden Sprachdateien und einen Eintrag im `enum` von `sources` in der
OpenAPI-Beschreibung.

## 4. Was das Modul tut

### 4.1 Netze

Eingeschaltete DHCP-Netze aus DNS64 erscheinen von selbst. Eigene Netze kommen aus `ipam_networks`. Gibt es zu
einem DHCP-Netz eine eigene Zeile mit demselben Netz, ergänzt sie Name, VLAN, Schnittstelle und Router; der
DHCP-Bereich kommt dann weiter aus DNS64. Ohne Angabe wird das VLAN aus dem Namen der Schnittstelle gelesen
(`eth0.20`, `vlan20`). Netze dürfen sich nicht überschneiden.

### 4.2 Art und Zustand einer Adresse

| Art | Bedeutung |
|---|---|
| `fixed` | feste Adresse im DHCP-Server von DNS64 |
| `dhcp` | aktive Vergabe des DHCP-Servers |
| `static` | alles andere: von Hand eingetragen oder nur aus DNS, Checks, Monitor64, PROXY64 oder dem Scan bekannt |
| `conflict` | siehe 4.4 |

Zustand: `online`, wenn die letzte Sichtung höchstens 10 Minuten alt ist, sonst `offline`; `unknown`, wenn es
nie eine gab; `conflict` geht vor.

### 4.3 Nachbartabelle und Netz-Scan

Solange das Modul läuft, liest es alle 2 Minuten die Nachbartabelle des Hosts (Netlink `RTM_GETNEIGH`, IPv4) und
merkt je IP und MAC, wann der Kernel die Adresse zuletzt bestätigt hat (`nda_cacheinfo`, Feld `confirmed`). Feste,
gescheiterte und unvollständige Einträge zählen nicht. Gemerkt werden nur Adressen in bekannten Netzen, 30 Tage lang.

Der Netz-Scan schickt jeder Adresse eines Netzes ein einzelnes UDP-Paket mit einem Byte an Port 9 („discard“).
Dafür fragt der Kernel die MAC per ARP an; nach 1,5 Sekunden Wartezeit wird die Nachbartabelle gelesen. Es werden
keine Ports abgetastet und keine Rohsockets benutzt.

- Nur für Netze, in denen die Node selbst eine Adresse hat. Im Docker-Bridge-Netz und bei gerouteten VLANs ist der
  Knopf gesperrt (`ErrNotAttached`); dafür braucht es `network_mode: host` oder macvlan.
- Ein Scan zur Zeit (`scanMu`). 2 ms Pause je Adresse: ein /24 dauert rund 2 Sekunden, ein /20 rund 10 Sekunden.
  Der Knopf auf der Seite und `POST /api/ipam64/scan` warten auf das Ergebnis.
- Automatisch alle 5, 15 oder 60 Minuten (Einstellung auf der Seite, Standard aus) über alle angeschlossenen Netze.

### 4.4 Konflikte

Ein Konflikt liegt vor, wenn in den letzten 15 Minuten zwei verschiedene MAC-Adressen an einer IP bestätigt wurden
oder eine bestätigte MAC nicht zu der zugewiesenen passt (feste Adresse, Vergabe oder von Hand eingetragene MAC).
Der Konflikt verschwindet von selbst, wenn die zweite MAC 15 Minuten lang nicht mehr auftaucht. Es gibt dazu keine
Benachrichtigung.

Die Schwellen stehen als Konstanten in `module.go` (`pollEvery`, `onlineFor`, `conflictFor`, `seenKeep`) und in
`scan.go` (`scanPause`, `scanWait`).

## 5. Daten

### 5.1 Tabellen (`internal/ipam64/migrations/0001_init.sql`)

| Tabelle | Inhalt |
|---|---|
| `ipam_networks` | eigene Netze bzw. Ergänzungen zu DHCP-Netzen; Schlüssel ist das Netz (`subnet`, eindeutig) |
| `ipam_addresses` | von Hand eingetragene Adressen (`ip` eindeutig, MAC, Name, Bemerkung) |
| `ipam_seen` | Sichtungen aus der Nachbartabelle je IP und MAC (`first_seen`, `last_seen`), 30 Tage |

Zeiten sind Unix-Sekunden. Die Migration läuft wie bei jedem Modul über `store.Migrate` mit dem Scope `ipam64`.

### 5.2 Einstellungen des Kerns

| Schlüssel | Inhalt |
|---|---|
| `ipam64.autoscan` | Abstand des automatischen Scans in Minuten (`0`, `5`, `15`, `60`) |
| `ipam64.lastscan` | Zeitpunkt des letzten Scans (Unix-Sekunden) |

### 5.3 Backup, Wiederherstellung, Zurücksetzen

Die Tabellen liegen in der Datenbank der Node und laufen mit. `storetest.Scopes` kennt den Scope `ipam64`; die Tests
für Backup und Wiederherstellung (`internal/web`) und für das Zurücksetzen (`cmd/node64`) laufen damit grün.

## 6. Oberfläche und API

Eine Seite, `templates/pages/overview.html`, ohne eigenes JavaScript: Filter, Suche, Abschnitt der Adresskarte und
die Formulare laufen über die Abfrage (`net`, `kind`, `q`, `block`, `form`, `edit`, `ip`).

| Route | Zweck |
|---|---|
| `GET /ipam64` | Übersicht |
| `GET /ipam64/export.csv?net=` | CSV des Netzes bzw. aller Netze; Werte, die mit `=`, `+`, `-`, `@` beginnen, werden entschärft |
| `POST /ipam64/network`, `/ipam64/network/delete` | Netz speichern, löschen |
| `POST /ipam64/address`, `/ipam64/address/delete` | Adresse speichern, löschen |
| `POST /ipam64/scan`, `/ipam64/autoscan` | Netz-Scan, automatischer Scan |
| `GET /api/ipam64/networks` | Netze mit Belegung |
| `GET /api/ipam64/addresses?network=` | belegte Adressen |
| `POST /api/ipam64/scan` | Netz-Scan (`{"network":"192.168.1.0/24"}`) |

Alle Routen laufen über `g.Enabled(g.Auth(...))`. Die API steht in `openapi.json` unter dem Tag `IPAM64`. Schreibende
Aktionen landen im Audit-Log (`ipam64.network.save`, `ipam64.network.delete`, `ipam64.address.save`,
`ipam64.address.delete`, `ipam64.scan`, `ipam64.autoscan`). Das Modul liefert eine Dashboard-Karte
(`module.CardProvider`).

Die Farben der Adresskarte kommen aus den Diagrammfarben des Designs (`--n64-chart-1`, `--n64-chart-2`) und
Bootstrap-Variablen; beide Designs und Hell/Dunkel sind damit abgedeckt. Der CSS-Block enthält außerdem eine
allgemeine Regel `tr:target > td` (hebt die angesprungene Zeile hervor), die für alle Tabellen der Oberfläche gilt.

## 7. Prüfen

```bash
gofmt -l cmd internal
go vet ./cmd/... ./internal/ipam64/
go test ./internal/ipam64/
go test ./...
```

`internal/ipam64/module_test.go` deckt ab: Zusammenführen der Quellen (Arten, Namen, Zustand, Konflikt),
Netz-Scan mit ersetzter Nachbartabelle, den Netlink-Parser und die Seiten samt Formularen, CSV und API über den
Kern (aus = 404 und kein Menüpunkt).

Geprüft wurde bisher nur mit Testdaten und im Browser mit Beispieldaten (beide Farbschemata, Handy). **Nicht
geprüft** ist der Betrieb an einem echten Netz. Vor dem Release auf einer Node mit `network_mode: host` ansehen:

1. Modul einschalten: DHCP-Netze aus DNS64 erscheinen, Vergaben und feste Adressen stimmen mit *DNS64 › DHCP* überein.
2. „Netz scannen“: Die Zahl der Antworten ist plausibel, Geräte werden „Online“.
3. Nach einigen Minuten ohne Scan: „Zuletzt gesehen“ läuft weiter, Geräte ohne Verkehr zur Node werden „Offline“.
4. Zwei Geräte mit derselben IP: Die Zeile wird zum Konflikt und nennt beide MAC-Adressen.
5. Im Docker-Bridge-Netz: Der Knopf ist gesperrt und der Hinweis erscheint.
6. Als Benutzer 65532 im Image: Nachbartabelle und Scan gehen ohne zusätzliche Capabilities.

## 8. Offene Punkte und Entscheidungen

Bewusst nicht enthalten:

- **Cluster:** Eigene Netze, Adressen und Sichtungen werden nicht abgeglichen; jede Node führt ihre eigene Sicht.
- **Export/Import der Konfiguration:** `module.Exporter` ist nicht umgesetzt, eigene Netze und Adressen fehlen im
  JSON-Export. Im vollständigen Backup der Datenbank sind sie enthalten.
- **Benachrichtigung** bei Konflikten, **Prometheus**-Kennzahlen und **MCP**-Werkzeuge gibt es nicht.
- **IPv6** fehlt ganz.

Zu entscheiden:

- **Art des Scans.** UDP an Port 9 plus Nachbartabelle kommt ohne Rechte aus. Die Alternative wäre echtes ARP über
  einen Rohsocket (`cap_net_raw` hat das Image schon); das wäre unabhängig von der Nachbartabelle, aber mehr Code.
- **Große Netze.** Ein /20 hat 4094 Adressen, die Nachbartabelle des Kernels fasst in der Voreinstellung 1024
  Einträge (`gc_thresh3`). Ob ein Scan so großer Netze vollständig ist, ist nicht gemessen. Wer sichergehen will,
  setzt `minPrefix` in `store.go` auf 22.
- **„Adresse reservieren“** trägt nur in IPAM64 ein. Naheliegend wäre, von dort direkt eine feste Adresse im
  DHCP-Server anzulegen (`dns64.AddDHCPReservation`).
- **Reihenfolge der Namen** (DHCP vor lokalem DNS) und die **Schwellen** aus 4.3 und 4.4.
- **Menü und Symbol:** `Order: 45`, `bi-grid-3x3-gap`, kein eigenes Logo unter `/static/img`.
- **Projektstruktur im README** (Abschnitt „Entwicklung“) nennt das Paket noch nicht.

Beim Release wie üblich: Datum in `internal/ipam64/CHANGELOG.md` und `CHANGELOG.en.md` anpassen und den Stichpunkt
im öffentlichen Changelog unter die Version ziehen.
