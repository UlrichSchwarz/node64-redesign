-- IPAM64 0.7.0: Adresse des fremden DHCP-Servers im gemeldeten Netz (leer = nicht gemeldet), fuer die Kennzeichnung im
-- Raster und in der Liste.

ALTER TABLE ipam_source_networks ADD COLUMN server TEXT NOT NULL DEFAULT '';
