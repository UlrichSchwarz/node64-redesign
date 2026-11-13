# Changes – IPAM64

## 0.1.0 – 2026-11-13

First version of the module (off by default, IPv4 only).

### New
- Overview of IP usage per network: key figures, network list with usage bars, address map and address list with
  filter by type and search.
- Merges what the node already knows: DHCP networks, leases and fixed addresses as well as local DNS records from
  DNS64, targets of Healthchecks, hosts from Monitor64, targets of PROXY64 and the neighbour table (ARP) – column
  “Known from”.
- Add your own networks (name, VLAN, interface, router, DHCP range of another server) or add name and VLAN to DHCP
  networks from DNS64.
- Reserve addresses manually (name, MAC, comment), also by clicking a free cell of the address map.
- Network scan of directly attached networks via ARP, on demand or automatically every 5, 15 or 60 minutes.
- Conflicts: two MAC addresses on one IP, or a different MAC than assigned.
- CSV export and API (`GET /api/ipam64/networks`, `GET /api/ipam64/addresses`, `POST /api/ipam64/scan`).
- Dashboard card with networks, used addresses and conflicts.
