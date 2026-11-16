<?php
/* Node64 IPAM64 pfSense package. Apache-2.0. */
##|+PRIV
##|*IDENT=page-services-node64ipam
##|*NAME=WebCfg - Services: Node64 IPAM64
##|*DESCR=Manage the Node64 IPAM64 DHCP collector
##|*MATCH=packages/node64ipam/index.php*
##|-PRIV
require_once('guiconfig.inc');
require_once('/usr/local/pkg/node64ipam.inc');

$s = node64ipam_settings();
$s += ['url'=>'', 'key'=>'', 'source'=>'pfsense', 'interval'=>'5', 'enabled'=>'no'];
$errors = [];
$message = '';
$result = [];
if ($_SERVER['REQUEST_METHOD'] === 'POST') {
    $action = (string)($_POST['action'] ?? '');
    if ($action === 'save') {
        $candidate = [
            'url' => trim((string)($_POST['url'] ?? '')),
            'key' => trim((string)($_POST['key'] ?? '')) ?: (string)$s['key'],
            'source' => trim((string)($_POST['source'] ?? '')),
            'interval' => (string)($_POST['interval'] ?? ''),
            'enabled' => isset($_POST['enabled']) ? 'yes' : 'no',
        ];
        $errors = node64ipam_validate($candidate);
        if ($candidate['enabled'] === 'yes' && node64ipam_legacy_jobs() > 0) $errors[] = 'Der bisherige /root/node64-ipam.php-Cron-Eintrag ist noch vorhanden. Bitte zuerst in Services → Cron entfernen, um doppelte Meldungen zu vermeiden.';
        if (!$errors) {
            $stored = $candidate;
            $stored['api_token'] = $stored['key'];
            unset($stored['key']);
            config_set_path(NODE64IPAM_CONFIG, [$stored]);
            write_config('Node64 IPAM64 settings changed');
            node64ipam_sync();
            $s = $candidate;
            $message = 'Einstellungen gespeichert.';
        }
    } elseif ($action === 'preview') {
        $result = node64ipam_run(true);
    } elseif ($action === 'send') {
        $result = node64ipam_run(false);
    }
}
$status = node64ipam_status();
$pgtitle = [gettext('Services'), 'Node64 IPAM64'];
include('head.inc');
if ($errors) print_input_errors($errors);
function node64ipam_h($s): string { return htmlspecialchars((string)$s, ENT_QUOTES | ENT_SUBSTITUTE, 'UTF-8'); }
?>
<div class="panel panel-default">
 <div class="panel-heading"><h2 class="panel-title">Node64 IPAM64</h2></div>
 <div class="panel-body">
  <p>DHCPv4-Netze, feste Adressen und Kea-Vergaben an eine Node64 melden. DHCP und Kea werden nur gelesen.</p>
  <?php if ($message): ?><div class="alert alert-success"><?=node64ipam_h($message)?></div><?php endif; ?>
  <?php if ($result): ?><div class="alert alert-<?=!empty($result['ok']) ? 'success' : 'danger'?>"><?=node64ipam_h($result['message'])?><?php if (isset($result['networks'])): ?>: <?= (int)$result['networks'] ?> Netze, <?= (int)$result['reservations'] ?> feste Adressen, <?= (int)$result['leases'] ?> Vergaben<?php if (!empty($result['skipped'])): ?>; <?= (int)$result['skipped'] ?> übersprungen (<?=node64ipam_h(implode(', ', $result['reasons'] ?? []))?>)<?php endif; ?><?php endif; ?></div><?php endif; ?>
  <form method="post" class="form-horizontal">
   <div class="form-group"><label class="col-sm-3 control-label" for="url">Node64-Adresse</label><div class="col-sm-9"><input class="form-control" id="url" name="url" type="url" required value="<?=node64ipam_h($s['url'])?>"><p class="help-block">Beispiel: https://node64.example.net:8080. HTTPS-Zertifikate werden geprüft.</p></div></div>
   <div class="form-group"><label class="col-sm-3 control-label" for="key">API-Key</label><div class="col-sm-9"><input class="form-control" id="key" name="key" type="password" autocomplete="new-password" value=""><p class="help-block">Leer lassen, um den gespeicherten Schlüssel beizubehalten. Der Schlüssel steht in der pfSense-Konfiguration und ihren Backups.</p></div></div>
   <div class="form-group"><label class="col-sm-3 control-label" for="source">Quellenkennung</label><div class="col-sm-9"><input class="form-control" id="source" name="source" value="<?=node64ipam_h($s['source'])?>" required maxlength="32"><p class="help-block">Bei mehreren Firewalls je eine eigene Kennung verwenden.</p></div></div>
   <div class="form-group"><label class="col-sm-3 control-label" for="interval">Intervall</label><div class="col-sm-9"><select class="form-control" id="interval" name="interval"><?php foreach (['1','5','10','15','30','60'] as $v): ?><option value="<?=$v?>" <?=((string)$s['interval']===$v?'selected':'')?>>Alle <?=$v?> Minuten</option><?php endforeach; ?></select></div></div>
   <div class="form-group"><div class="col-sm-offset-3 col-sm-9"><label><input type="checkbox" name="enabled" value="yes" <?=($s['enabled']==='yes'?'checked':'')?>> Regelmäßigen Abgleich aktivieren</label></div></div>
   <div class="form-group"><div class="col-sm-offset-3 col-sm-9"><button class="btn btn-primary" type="submit" name="action" value="save">Speichern</button></div></div>
  </form>
  <hr>
  <form method="post"><button class="btn btn-default" type="submit" name="action" value="preview">Vorschau ohne Senden</button> <button class="btn btn-default" type="submit" name="action" value="send">Jetzt einmal senden</button></form>
  <p class="help-block">Vorschau und Senden verwenden die gespeicherten Einstellungen. Ein Versand ersetzt den bisherigen Stand dieser Quellenkennung auf der Ziel-Node.</p>
  <h3>Letzter Versand</h3>
  <?php if (!$status): ?><p>Seit dem letzten Neustart liegt kein Ergebnis vor.</p><?php else: ?><p><?=node64ipam_h($status['time'] ?? '')?> — <?=node64ipam_h($status['message'] ?? '')?><?php if (isset($status['networks'])): ?>: <?=(int)$status['networks']?> Netze, <?=(int)$status['reservations']?> feste Adressen, <?=(int)$status['leases']?> Vergaben<?php endif; ?></p><?php endif; ?>
 </div>
</div>
<?php include('foot.inc'); ?>
