<?php
/* Node64 IPAM64 pfSense package. Apache-2.0. */
require_once('config.inc');
require_once('services.inc');

const NODE64IPAM_CONFIG = 'installedpackages/node64ipam/config';
const NODE64IPAM_JOB = '/usr/local/bin/php /usr/local/pkg/node64ipam-run.php';
if (!defined('NODE64IPAM_RUN_DIR')) define('NODE64IPAM_RUN_DIR', '/var/run/node64ipam');
if (!defined('NODE64IPAM_PHP')) define('NODE64IPAM_PHP', '/usr/local/bin/php');
if (!defined('NODE64IPAM_COLLECTOR')) define('NODE64IPAM_COLLECTOR', '/usr/local/pkg/node64ipam-collector.php');

function node64ipam_settings(): array {
    // pfSense serializes the "config" tag as a list of entries.
    $entries = config_get_path(NODE64IPAM_CONFIG, []);
    $entry = $entries[0] ?? null;
    if (!is_array($entry)) {
        return [];
    }
    // The XML parser also treats "key" as a list.
    $entry['key'] = $entry['api_token'] ?? '';
    unset($entry['api_token']);
    return $entry;
}
function node64ipam_validate(array $s, bool $need_key = true): array {
    $errors = [];
    $url = (string)($s['url'] ?? '');
    $p = parse_url($url);
    if (!is_array($p) || !in_array(strtolower($p['scheme'] ?? ''), ['http','https'], true) || empty($p['host']) || isset($p['user']) || isset($p['pass']) || isset($p['query']) || isset($p['fragment']) || preg_match('/[\x00-\x20\x7f]/', $url)) {
        $errors[] = 'Bitte eine HTTP- oder HTTPS-Adresse ohne Zugangsdaten, Query oder Fragment eingeben.';
    }
    if (!preg_match('/^[a-z0-9][a-z0-9_-]{0,31}$/', (string)($s['source'] ?? ''))) $errors[] = 'Die Quellenkennung darf nur Kleinbuchstaben, Ziffern, - und _ enthalten (maximal 32 Zeichen).';
    if (!in_array((string)($s['interval'] ?? ''), ['1','5','10','15','30','60'], true)) $errors[] = 'Ungültiges Intervall.';
    if ($need_key && !preg_match('/^node64_[A-Za-z0-9_-]+$/', (string)($s['key'] ?? ''))) $errors[] = 'Ein gültig aussehender Node64-API-Key fehlt.';
    return $errors;
}
function node64ipam_legacy_jobs(): int {
    $count = 0;
    foreach (config_get_path('cron/item', []) as $item) {
        if (str_contains((string)($item['command'] ?? ''), '/root/node64-ipam.php')) $count++;
    }
    return $count;
}
function node64ipam_sync(): void {
    $s = node64ipam_settings();
    $enabled = ($s['enabled'] ?? '') === 'yes' && !node64ipam_validate($s) && node64ipam_legacy_jobs() === 0;
    $n = (int)($s['interval'] ?? 5);
    $minute = $n === 60 ? '0' : '*/' . $n;
    install_cron_job(NODE64IPAM_JOB, $enabled, $minute, '*', '*', '*', '*', 'root');
}
function node64ipam_remove_schedule(): void { install_cron_job(NODE64IPAM_JOB, false); }
function node64ipam_status(): array {
    $p = NODE64IPAM_RUN_DIR . '/status.json';
    if (!is_readable($p)) return [];
    $j = json_decode((string)file_get_contents($p), true);
    return is_array($j) ? $j : [];
}
function node64ipam_run(bool $preview): array {
    $s = node64ipam_settings();
    $errors = node64ipam_validate($s, !$preview);
    if ($errors) return ['ok'=>false, 'message'=>implode(' ', $errors)];
    if (!is_dir(NODE64IPAM_RUN_DIR) && !@mkdir(NODE64IPAM_RUN_DIR, 0700, true) && !is_dir(NODE64IPAM_RUN_DIR)) return ['ok'=>false,'message'=>'Laufzeitverzeichnis kann nicht angelegt werden.'];
    @chmod(NODE64IPAM_RUN_DIR, 0700);
    $lock = fopen(NODE64IPAM_RUN_DIR . '/run.lock', 'c');
    if (!$lock || !flock($lock, LOCK_EX | LOCK_NB)) return ['ok'=>false,'message'=>'Ein Abgleich läuft bereits.'];
    $path = tempnam(NODE64IPAM_RUN_DIR, 'config-');
    if ($path === false) { flock($lock, LOCK_UN); fclose($lock); return ['ok'=>false,'message'=>'Temporäre Einstellungen können nicht angelegt werden.']; }
    chmod($path, 0600);
    $result = ['ok'=>false, 'message'=>'Sammler konnte nicht gestartet werden.'];
    try {
        $config = '';
        foreach (['url','key','source'] as $field) {
            $v = (string)($s[$field] ?? '');
            $config .= $field . ' = "' . addcslashes($v, "\\\"") . "\"\n";
        }
        $config .= "label = \"pfSense\"\n";
        if (file_put_contents($path, $config, LOCK_EX) === false) throw new RuntimeException('Temporäre Einstellungen können nicht gespeichert werden.');
        $command = [NODE64IPAM_PHP, NODE64IPAM_COLLECTOR, '-c', $path, $preview ? '-n' : '-v'];
        $pipes = [];
        $proc = proc_open($command, [['pipe','r'],['pipe','w'],['pipe','w']], $pipes);
        if (!is_resource($proc)) throw new RuntimeException('Sammler konnte nicht gestartet werden.');
        fclose($pipes[0]);
        $out = stream_get_contents($pipes[1]); fclose($pipes[1]);
        $err = stream_get_contents($pipes[2]); fclose($pipes[2]);
        $rc = proc_close($proc);
        if ($rc !== 0) {
            /* Never include a server response, token, hostnames, or collected records in status. */
            if (preg_match('/Node64 antwortet mit ([0-9]{3})/', $err, $http)) {
                $message = 'Node64 meldet HTTP ' . $http[1] . '.';
            } elseif (str_contains($err, 'Lease-Datei')) {
                $message = 'Kea-Lease-Datei fehlt oder ist ungültig; nichts wurde gesendet.';
            } else {
                $message = 'Sammler fehlgeschlagen (Exit-Code ' . $rc . '). Node-Adresse, API-Key und Erreichbarkeit prüfen.';
            }
            $result = ['ok'=>false,'message'=>$message];
        } elseif ($preview) {
            $json = json_decode($out, true);
            if (!is_array($json) || !isset($json['networks'], $json['reservations'], $json['leases'])) throw new RuntimeException('Ungültige Vorschau vom Sammler.');
            $result = ['ok'=>true,'message'=>'Vorschau ohne Übermittlung','networks'=>count($json['networks']),'reservations'=>count($json['reservations']),'leases'=>count($json['leases'])];
        } else {
            if (!preg_match('/(\d+) Netze, (\d+) feste Adressen, (\d+) Vergaben/', $out, $m)) throw new RuntimeException('Ungültige Antwort vom Sammler.');
            $result = ['ok'=>true,'message'=>'An Node64 gemeldet','networks'=>(int)$m[1],'reservations'=>(int)$m[2],'leases'=>(int)$m[3],'skipped'=>preg_match('/(\d+) Eintraege uebersprungen/', $err, $sk) ? (int)$sk[1] : 0];
            if ($result['skipped']) {
                $reasons = [];
                foreach (['no ipv4 address'=>'keine IPv4-Adresse', 'invalid mac'=>'ungültige MAC-Adresse', 'duplicate'=>'Doppeleintrag', 'prefix'=>'ungültiges Netzpräfix'] as $needle=>$label) if (stripos($err, $needle) !== false) $reasons[] = $label;
                $result['reasons'] = $reasons ?: ['siehe Node64-Protokoll'];
            }
        }
    } catch (Throwable $e) { $result = ['ok'=>false,'message'=>$e->getMessage()]; }
    finally { @unlink($path); }
    if (!$preview) {
        $result['time'] = gmdate('c');
        $tmp = tempnam(NODE64IPAM_RUN_DIR, 'status-');
        if ($tmp !== false) { chmod($tmp,0600); if (file_put_contents($tmp,json_encode($result),LOCK_EX)!==false) rename($tmp,NODE64IPAM_RUN_DIR.'/status.json'); else @unlink($tmp); }
    }
    flock($lock, LOCK_UN); fclose($lock);
    return $result;
}
