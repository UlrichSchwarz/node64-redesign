<?php
require_once('/usr/local/pkg/node64ipam.inc');
$r = node64ipam_run(false);
if (empty($r['ok'])) { error_log('Node64 IPAM64: ' . $r['message']); exit(1); }
