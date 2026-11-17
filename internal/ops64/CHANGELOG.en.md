# OPS64 – Changes

## 0.2.0 – unreleased

New:

- Backups via the Proxmox API (off by default, switch on the page): the node reads backup jobs and their runs itself from Proxmox VE (`/cluster/backup`, tasks of type `vzdump`) and Proxmox Backup Server (sync, verify and prune jobs, garbage collection) – using the accesses from Monitor64, read-only, every 5 minutes. Nothing needs to be installed on the hosts. Card with state and error per access, "Poll now".
- Add a host with one line for the host's shell (download script, settings, cron, first report); plus the line to remove it. With API polling on, the collector only reports cron jobs and timers (`skip = pve,pbs`).
- The collector script can be downloaded with the API key (`GET /api/ops64/collector/linux.py`).

OPS64 still only reads: it changes nothing on the hosts and sends read requests only via the API.

## 0.1.0 – unreleased

New (design and implementation from the community, UlrichSchwarz):

- Operations & maintenance in one place: hosts report their backup jobs, cron jobs and timers with their runs to the node (`PUT /api/ops64/sources/<name>`). OPS64 only reads and changes nothing on the hosts.
- Collector for Linux hosts (Python 3, no extra packages, via cron): backup jobs and backup runs of Proxmox VE, jobs of Proxmox Backup Server, systemd timers with their last run and all crontabs. Passwords and tokens in command lines are masked before sending. Guide and download under *OPS64 → Add host*.
- Schedule by day, week and month: one row per task with bars on the time axis (night and morning or whole day) and the state per weekday; states planned, running, successful, failed, overdue, no feedback. Filters by host, type and status.
- The node reads the schedules itself: cron (including `@daily` …) and calendar expressions of systemd and Proxmox (`mon..fri 02:00`, `*-*-01 04:00:00`, `daily`), in the host's time zone.
- Expected duration from the last successful runs (median; the longest if they vary a lot) or set by hand; otherwise "duration unclear".
- Concurrent tasks: overlaps across all hosts as a red band in the schedule, with the tasks involved.
- Details and history per task; tasks can be hidden and shown (system tasks are hidden by default).
- Lists "Backups" and "Cron jobs & timers" with last and next run.
- Alerts via the default channels, switchable per kind: failure, overdue task, unusually long run, tasks planned at the same time, host stopped reporting.
- API `/api/ops64/…` (hosts, tasks, schedule), dashboard card, audit area "OPS64".

Not included yet: shared control ("no concurrent jobs" – each job waits for the previous one), hints on LXC updates and firmware.
