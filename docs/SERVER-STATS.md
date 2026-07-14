# Admin server statistics

Settings → **Server** gives workspace admins a quick operational snapshot of the
Fleet runtime host without requiring SSH. It refreshes every 10 seconds and
shows:

- CPU utilization between samples, logical cores, and 1/5/15-minute load;
- memory and swap use;
- root-filesystem use;
- aggregate receive/transmit rates and totals for non-loopback interfaces; and
- hostname, platform, and host uptime.

The chat server reads Linux procfs and `statfs` directly; it does not install or
run an agent. Missing sources produce a partial response and an explanatory UI
warning rather than failing the page. In a containerized deployment, the values
describe the namespaces and root filesystem visible to Fleet, which may differ
from the physical node.

`GET /admin/server-stats` is protected by the normal authenticated admin-role
middleware. The Next.js route `/api/admin/server-stats` forwards the signed-in
session and does not possess or expose the deployment admin API key. The response
intentionally excludes process lists, command lines, environment variables, IP
addresses, mount inventories, and file names.

This is a convenience status view, not a monitoring system. Use
Prometheus/node_exporter or the deployment's existing observability stack for
history, alerting, per-process analysis, and multi-node aggregation.
