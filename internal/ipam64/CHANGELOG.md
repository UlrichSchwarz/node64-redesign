# Änderungen – IPAM64

## 0.1.0 – 2026-11-13

Erste Fassung des Moduls (standardmäßig aus, nur IPv4).

### Neu
- Übersicht der IP-Belegung je Netz: Kennzahlen, Netzliste mit Belegungsbalken, Adresskarte und Belegungsliste mit
  Filter nach Art und Suche.
- Führt zusammen, was die Node schon weiß: DHCP-Netze, Vergaben und feste Adressen sowie lokale DNS-Einträge aus DNS64,
  Ziele der Healthchecks, Hosts aus Monitor64, Ziele von PROXY64 und die Nachbartabelle (ARP) – Spalte „Bekannt aus“.
- Eigene Netze anlegen (Name, VLAN, Schnittstelle, Router, DHCP-Bereich eines fremden Servers) bzw. DHCP-Netze aus
  DNS64 um Name und VLAN ergänzen.
- Adressen von Hand reservieren (Name, MAC, Bemerkung), auch per Klick auf ein freies Kästchen der Adresskarte.
- Netz-Scan direkt angeschlossener Netze per ARP, auf Knopfdruck oder automatisch alle 5, 15 oder 60 Minuten.
- Konflikte: zwei MAC-Adressen an einer IP bzw. eine andere MAC als zugewiesen.
- CSV-Export und API (`GET /api/ipam64/networks`, `GET /api/ipam64/addresses`, `POST /api/ipam64/scan`).
- Karte im Dashboard mit Netzen, belegten Adressen und Konflikten.
