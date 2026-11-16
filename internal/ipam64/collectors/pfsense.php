#!/usr/local/bin/php
<?php
/*
 * Node64 IPAM64 – Sammler fuer pfSense (DHCPv4 mit Kea).
 *
 * Liest die DHCP-Netze und festen Adressen aus der Konfiguration der pfSense und die aktuellen Vergaben aus den
 * Lease-Dateien von Kea und meldet alles an IPAM64 (PUT /api/ipam64/sources/<name>). Jede Meldung ersetzt den
 * bisherigen Stand dieser Quelle. Das Skript liest nur; an der pfSense aendert es nichts.
 *
 * Einrichtung:
 *   1. Diese Datei als /root/node64-ipam.php ablegen.
 *   2. /root/node64-ipam.ini anlegen (Rechte 600):
 *        url = "http://192.168.1.2:8080"      ; Adresse der Node64-Oberflaeche
 *        key = "node64_..."                   ; API-Key aus Node64 (Einstellungen)
 *   3. Probelauf ohne Senden:   php /root/node64-ipam.php -n
 *      Senden mit Ausgabe:      php /root/node64-ipam.php -v
 *   4. Regelmaessig ausfuehren (Paket "Cron", z. B. alle 5 Minuten):
 *        /usr/local/bin/php /root/node64-ipam.php
 *
 * Weitere Angaben in der .ini (alle optional):
 *   source = "pfsense"           Kennung der Quelle (a-z, 0-9, - und _), bei mehreren Firewalls je eine eigene
 *   label = "pfSense"            Anzeige in IPAM64
 *   verify_tls = 1               0 = Zertifikat der Node nicht pruefen (selbst signiert)
 *   cafile = ""                  eigene CA-Datei fuer die Pruefung
 *   config_xml = "/cf/conf/config.xml"
 *   kea_conf = "/usr/local/etc/kea/kea-dhcp4.conf"
 *   lease_file = ""              leer = aus der Kea-Konfiguration, sonst die neueste bekannte Datei
 *
 * Aufruf: php node64-ipam.php [-c datei.ini] [-n] [-v]
 *   -n  nur anzeigen, was gesendet wuerde
 *   -v  Ergebnis ausgeben (ohne -v bleibt das Skript bei Erfolg still, damit Cron keine Mails schickt)
 */

const N64_VERSION = '2';

function n64_fail(string $msg): void
{
    fwrite(STDERR, "node64-ipam: $msg\n");
    exit(1);
}

function n64_ip4(string $s): bool
{
    return filter_var($s, FILTER_VALIDATE_IP, FILTER_FLAG_IPV4) !== false;
}

/* Netzadresse zu Adresse und Praefixlaenge, z. B. 192.168.1.1/24 -> 192.168.1.0/24. */
function n64_subnet(string $ip, int $bits): string
{
    $mask = $bits === 0 ? 0 : (0xFFFFFFFF << (32 - $bits)) & 0xFFFFFFFF;
    return long2ip(ip2long($ip) & $mask) . '/' . $bits;
}

/* Netze und feste Adressen aus der Konfiguration der pfSense. */
function n64_read_config(string $path): array
{
    if (!function_exists('simplexml_load_file')) {
        n64_fail('PHP-Erweiterung SimpleXML fehlt');
    }
    if (!is_readable($path)) {
        n64_fail("Konfiguration nicht lesbar: $path");
    }
    $xml = @simplexml_load_file($path);
    if ($xml === false) {
        n64_fail("Konfiguration nicht auswertbar: $path");
    }
    $networks = [];
    $reservations = [];
    if (!isset($xml->dhcpd)) {
        return [$networks, $reservations, (string)($xml->dhcpbackend ?? '')];
    }
    foreach ($xml->dhcpd->children() as $ifname => $d) {
        if (!isset($d->enable) || !isset($xml->interfaces->{$ifname})) {
            continue; // DHCP auf dieser Schnittstelle aus
        }
        $if = $xml->interfaces->{$ifname};
        $ip = trim((string)$if->ipaddr);
        $bits = (int)trim((string)$if->subnet);
        if (!n64_ip4($ip) || $bits < 1 || $bits > 32) {
            continue; // Schnittstelle ohne feste IPv4-Adresse
        }
        $name = trim((string)$if->descr);
        $net = [
            'subnet' => n64_subnet($ip, $bits),
            'name' => $name !== '' ? $name : strtoupper((string)$ifname),
            'interface' => trim((string)$if->if),
        ];
        $net['server'] = $ip; // Kea antwortet mit der Adresse der Schnittstelle
        $gw = trim((string)$d->gateway);
        $net['router'] = n64_ip4($gw) ? $gw : ($gw === 'none' ? '' : $ip);
        $from = trim((string)$d->range->from);
        $to = trim((string)$d->range->to);
        if (n64_ip4($from) && n64_ip4($to)) {
            $net['range_start'] = $from;
            $net['range_end'] = $to;
        }
        $networks[] = $net;
        foreach ($d->staticmap as $m) {
            $sip = trim((string)$m->ipaddr);
            if (!n64_ip4($sip)) {
                continue; // Zuordnung nur ueber die MAC, ohne feste Adresse
            }
            $host = trim((string)$m->hostname);
            $descr = trim((string)$m->descr);
            $reservations[] = [
                'ip' => $sip,
                'mac' => strtolower(trim((string)$m->mac)),
                'name' => $host !== '' ? $host : $descr,
                'comment' => $host !== '' ? $descr : '',
            ];
        }
    }
    return [$networks, $reservations, (string)($xml->dhcpbackend ?? '')];
}

/* Lease-Datei von Kea: aus der Kea-Konfiguration, sonst die neueste der ueblichen Dateien. */
function n64_lease_file(array $ini): string
{
    if (!empty($ini['lease_file'])) {
        return (string)$ini['lease_file'];
    }
    $conf = @file_get_contents((string)($ini['kea_conf'] ?? '/usr/local/etc/kea/kea-dhcp4.conf'));
    if ($conf !== false) {
        $j = json_decode($conf, true);
        $db = $j['Dhcp4']['lease-database'] ?? null;
        if (is_array($db) && ($db['type'] ?? 'memfile') === 'memfile' && !empty($db['name'])) {
            return (string)$db['name'];
        }
        if (is_array($db) && ($db['type'] ?? 'memfile') !== 'memfile') {
            n64_fail('Kea speichert die Vergaben nicht in einer Datei (lease-database ' . $db['type'] . '), das kann dieses Skript nicht lesen');
        }
    }
    $best = '';
    foreach (['/var/db/kea/dhcp4.leases', '/var/lib/kea/dhcp4.leases'] as $f) {
        if (is_file($f) && ($best === '' || filemtime($f) > filemtime($best))) {
            $best = $f;
        }
    }
    return $best;
}

/*
 * Aktuelle Vergaben aus den Lease-Dateien von Kea. Kea haengt jede Aenderung an die Datei an und raeumt sie
 * regelmaessig auf (Lease File Cleanup). Gelesen wird in derselben Reihenfolge wie Kea beim Start:
 * <datei>.completed, sonst <datei>.2 und <datei>.1, zuletzt die Datei selbst; je Adresse gilt die letzte Zeile.
 */
function n64_read_leases(string $file, int $now): array
{
    if ($file === '' || !is_file($file)) {
        return [];
    }
    $files = is_file("$file.completed") ? ["$file.completed"] : array_filter(["$file.2", "$file.1"], 'is_file');
    $files[] = $file;
    $last = [];
    foreach ($files as $f) {
        $h = @fopen($f, 'r');
        if ($h === false) {
            continue;
        }
        $cols = null;
        while (($line = fgets($h)) !== false) {
            $row = explode(',', rtrim($line, "\r\n"));
            if ($cols === null) { // Kopfzeile: Spalten ueber den Namen finden (die Reihenfolge wechselt mit der Kea-Version)
                $cols = array_flip($row);
                if (!isset($cols['address'], $cols['hwaddr'], $cols['valid_lifetime'], $cols['expire'])) {
                    break;
                }
                continue;
            }
            $addr = $row[$cols['address']] ?? '';
            if ($addr !== '') {
                $last[$addr] = [$row, $cols];
            }
        }
        fclose($h);
    }
    $out = [];
    foreach ($last as $addr => [$row, $cols]) {
        $col = function (string $name) use ($row, $cols): string {
            return isset($cols[$name]) ? (string)($row[$cols[$name]] ?? '') : '';
        };
        $life = (int)$col('valid_lifetime');
        $expire = (int)$col('expire');
        $state = $col('state');
        $mac = strtolower($col('hwaddr'));
        // state 0 = vergeben; Laufzeit 0 = zurueckgegeben; 4294967295 = ohne Ablauf
        if (!n64_ip4($addr) || $mac === '' || $life <= 0 || ($state !== '' && $state !== '0')) {
            continue;
        }
        $infinite = $life >= 0xFFFFFFFF;
        if (!$infinite && $expire <= $now) {
            continue;
        }
        $host = rtrim(str_replace('&#x2c', ',', $col('hostname')), '.');
        $lease = ['ip' => $addr, 'mac' => $mac, 'name' => $host];
        if (!$infinite) {
            $lease['expires'] = $expire;
            $lease['last_seen'] = $expire - $life; // letzte Verlaengerung durch das Geraet
        }
        $out[] = $lease;
    }
    return $out;
}

function n64_send(array $ini, string $source, string $json): array
{
    $url = rtrim((string)$ini['url'], '/') . '/api/ipam64/sources/' . rawurlencode($source);
    if (!function_exists('curl_init')) {
        n64_fail('PHP-Erweiterung curl fehlt');
    }
    $c = curl_init($url);
    $verify = !isset($ini['verify_tls']) || (string)$ini['verify_tls'] !== '0';
    $opts = [
        CURLOPT_CUSTOMREQUEST => 'PUT',
        CURLOPT_POSTFIELDS => $json,
        CURLOPT_HTTPHEADER => ['Content-Type: application/json', 'Authorization: Bearer ' . $ini['key'], 'User-Agent: node64-ipam-pfsense/' . N64_VERSION],
        CURLOPT_RETURNTRANSFER => true,
        CURLOPT_CONNECTTIMEOUT => 10,
        CURLOPT_TIMEOUT => 30,
        CURLOPT_SSL_VERIFYPEER => $verify,
        CURLOPT_SSL_VERIFYHOST => $verify ? 2 : 0,
    ];
    if (!empty($ini['cafile'])) {
        $opts[CURLOPT_CAINFO] = (string)$ini['cafile'];
    }
    curl_setopt_array($c, $opts);
    $body = curl_exec($c);
    if ($body === false) {
        n64_fail('Node64 nicht erreichbar (' . $url . '): ' . curl_error($c));
    }
    $code = (int)curl_getinfo($c, CURLINFO_RESPONSE_CODE);
    curl_close($c);
    return [$code, (string)$body];
}

$opt = getopt('c:nv');
$iniFile = $opt['c'] ?? (dirname(__FILE__) . '/node64-ipam.ini');
$dry = isset($opt['n']);
$verbose = isset($opt['v']);
$ini = is_readable($iniFile) ? @parse_ini_file($iniFile) : false;
if ($ini === false) {
    if (!$dry) {
        n64_fail("Einstellungen nicht lesbar: $iniFile (url und key eintragen, siehe Kopf dieser Datei)");
    }
    $ini = [];
}
if (!$dry && (empty($ini['url']) || empty($ini['key']))) {
    n64_fail("in $iniFile fehlen url bzw. key");
}
$source = (string)($ini['source'] ?? 'pfsense');
if (!preg_match('/^[a-z0-9][a-z0-9_-]{0,31}$/', $source)) {
    n64_fail("source \"$source\" ist ungueltig (a-z, 0-9, - und _, hoechstens 32 Zeichen)");
}

[$networks, $reservations, $backend] = n64_read_config((string)($ini['config_xml'] ?? '/cf/conf/config.xml'));
$leases = [];
$note = '';
if ($backend !== '' && $backend !== 'kea') {
    $note = "DHCP-Server ist \"$backend\" statt Kea: Netze und feste Adressen werden gemeldet, Vergaben nicht";
} else {
    $leaseFile = n64_lease_file($ini);
    if ($leaseFile === '') {
        $note = 'keine Lease-Datei von Kea gefunden: Netze und feste Adressen werden gemeldet, Vergaben nicht';
    } else {
        $leases = n64_read_leases($leaseFile, time());
    }
}
if ($note !== '' && ($verbose || $dry)) {
    fwrite(STDERR, "node64-ipam: $note\n");
}

$json = json_encode([
    'label' => (string)($ini['label'] ?? 'pfSense'),
    'networks' => $networks,
    'reservations' => $reservations,
    'leases' => $leases,
], JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_INVALID_UTF8_SUBSTITUTE | ($dry ? JSON_PRETTY_PRINT : 0));
if ($json === false) {
    n64_fail('Meldung nicht darstellbar: ' . json_last_error_msg());
}
if ($dry) {
    echo $json, "\n";
    fwrite(STDERR, sprintf("node64-ipam: %d Netze, %d feste Adressen, %d Vergaben (nicht gesendet)\n", count($networks), count($reservations), count($leases)));
    exit(0);
}

[$code, $body] = n64_send($ini, $source, $json);
if ($code === 400 && strpos($body, 'unknown field') !== false) {
    n64_fail('Node64 ist aelter als dieses Skript (kennt ein Feld der Meldung nicht) - zuerst Node64 aktualisieren: ' . substr(trim($body), 0, 200));
}
if ($code !== 200) {
    n64_fail("Node64 antwortet mit $code: " . substr(trim($body), 0, 300));
}
$res = json_decode($body, true);
if ($verbose) {
    printf("node64-ipam: gemeldet als \"%s\": %d Netze, %d feste Adressen, %d Vergaben\n", $source, $res['networks'] ?? 0, $res['reservations'] ?? 0, $res['leases'] ?? 0);
}
if ($verbose && !empty($res['skipped'])) {
    fwrite(STDERR, sprintf("node64-ipam: %d Eintraege uebersprungen: %s\n", $res['skipped'], implode('; ', $res['skipped_notes'] ?? [])));
}
