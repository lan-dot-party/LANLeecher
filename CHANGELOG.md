# Changelog

All notable changes to this project are documented in this file.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Planned events:** under Events → Planned events, an event can be entered with its first and
  last day (optionally with start and end times). LANLeecher starts it at the start and stops it at
  the end by itself, to the second, and records both in the audit log as actor `schedule`. Planned
  events can be started early, deleted, and must not overlap; the dashboard shows the next one
  while no event runs. Also available as `lanleecher event schedule|schedules|unschedule` and as
  `GET/POST /api/v1/schedules` and `DELETE /api/v1/schedules/{id}`. Dates use the server's time
  zone (`TZ`).

## [1.1.0] - 2026-10-25

### Added

- Port groups are easier to name: the port's label is now called **display name** in the port
  list, ports whose traffic goes to a port group say so (and whether they are still unnamed), and
  port groups in rankings link to their port. The name appears in rankings, on the board and in
  the API.
- **Not an uplink:** a port flagged as "probably uplink" (many MACs or an LLDP neighbor) can be
  confirmed as not an uplink, e.g. a hypervisor with many VMs. The hint and the warning banner then
  stay off until the confirmation is withdrawn with "Check again" or the port becomes an uplink.
  Available per port and for a selection of ports; recorded in the audit log.
- Clicking into the traffic timeline of the dashboard or an event page shows which clients caused
  the traffic in that 5-minute interval; dragging selects a longer range. The detail lists the top
  10 by total (hidden entries included and marked, as the timeline counts them too), the totals and
  average rates of the range, unassigned traffic and a link to the full ranking for the range.

### Fixed

- Pressing Enter in a field of the port list saves the rows. It used to trigger "Apply to
  selection", so a changed label or role was lost.

## [1.0.1] - 2026-10-23

### Added

- Traffic charts show a tooltip with the time and the download and upload rate of the 5-minute
  interval under the mouse pointer (or finger).

### Fixed

- Forwarding-table source `auto` falls back to BRIDGE-MIB when Q-BRIDGE reports bridge ports that
  `dot1dBasePortIfIndex` does not list. TP-Link JetStream switches (e.g. T1500G-8T) report port
  numbers in `dot1qTpFdbPort`, so their FDB entries were dropped and no traffic was attributed to
  clients. The log now warns when FDB entries point to unknown interfaces.

## [1.0.0] - 2026-10-22

### Added

- Traffic accounting per client (MAC) and event via SNMP v2c/v3 with 64-bit counters: deltas only,
  with detection of switch reboots, counter resets and 64-bit wraps, and a plausibility check
  against the link speed.
- Attribution of port traffic to a single MAC, a port group (several clients on one port) or
  "unassigned"; uplinks are counted on the port only. Port roles `client`, `multi-client`, `server`
  and `uplink`.
- Forwarding-table sources `auto`, `qbridge`, `bridge` and `cisco-vlan`; LLDP neighbors and MAC
  counts as "probably uplink" hints. Interface classification by name for switches that report
  misleading `ifType` values (tested with TP-Link Omada/JetStream).
- IP and hostname discovery with history: SNMP ARP tables, the host's neighbor table, NetBIOS,
  mDNS, LLMNR and reverse DNS. Merge suggestions for MACs with the same hostname in an event.
- Events: start/stop, rankings per event and across all events, time ranges, comparison of up to
  three events, CSV and JSON export.
- Hiding ports and devices from rankings, evaluated at query time and retroactive.
- Web UI in English and German: dashboard with live throughput, rankings, search, device pages with
  per-event history and charts, switch and port management with connection test, merge
  suggestions, users, API tokens, audit log.
- Beamer view `/board` with live updates via Server-Sent Events.
- Kea DHCP as a source for IP addresses and client hostnames: Kea 2.x via the Control Agent and
  Kea 3.x via the servers' HTTP control sockets, read-only command allowlist, HTTPS with certificate
  verification during the handshake, one-click "trust this certificate" with a warning when it
  changes, optional mutual TLS. `tools/keasim` simulates Kea for trying it out.
- Vendor names for MAC addresses from the IEEE registries (MA-L, MA-M, MA-S), embedded in the
  binary and shown on device pages, port lists, search results and rankings, and as `vendor` in the
  client API. Randomized MACs get no vendor. `tools/genoui` refreshes the data.
- Light and dark theme: automatic (follows the operating system), or chosen per user with the
  toggle in the top bar or on the account page.
- REST API v1 with scoped bearer tokens (`read:rankings`, `read:clients`, `write:events`,
  `read:board`), IP allowlists, expiry, rate limits, RFC 9457 errors and an OpenAPI 3.1
  specification that tests enforce for every endpoint and error case; Prometheus metrics.
- Security: Argon2id passwords, login lockout, CSRF protection, strict Content Security Policy,
  AES-256-GCM encryption of SNMP credentials.
- CLI: `server`, `config init`, `migrate`, `backup`, `user`, `event`, `switch`, `version`.
- Packages for Debian/Ubuntu, Fedora/RHEL, Arch Linux and Alpine (amd64, arm64, armv7), a
  multi-arch Docker image, and a release workflow that installs every package in a fresh container
  before publishing.
- Documentation: installation, configuration reference and JSON Schema (generated), reverse proxy,
  LAN party guide, switch compatibility, API guide, architecture, security model, ADRs, examples,
  `llms.txt` and `llms-full.txt`.

[Unreleased]: https://github.com/lan-dot-party/LANLeecher/compare/v1.1.0...HEAD
[1.1.0]: https://github.com/lan-dot-party/LANLeecher/compare/v1.0.1...v1.1.0
[1.0.1]: https://github.com/lan-dot-party/LANLeecher/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/lan-dot-party/LANLeecher/releases/tag/v1.0.0
