# Installation

LANLeecher is a single binary with a built-in SQLite database. It needs no database server and no
agent on the clients – only SNMP access (UDP 161) to the switches.

## Requirements

- Linux on amd64, arm64 or armv7 (a Raspberry Pi is enough for a large LAN party).
- Switches that answer SNMP v2c or v3 with 64-bit interface counters (`ifHCInOctets`,
  `ifHCOutOctets`) and a forwarding table (BRIDGE-MIB or Q-BRIDGE-MIB). See
  [switch-compatibility.md](switch-compatibility.md).
- For hostnames: the LANLeecher host must reach the client IPs (NetBIOS UDP 137, mDNS UDP 5353,
  LLMNR UDP 5355). Multicast mDNS and the local neighbor table only cover the host's own segment.

## Packages

Every [release](https://github.com/lan-dot-party/LANLeecher/releases) carries packages for amd64,
arm64 and armv7, plus `SHA256SUMS` and a build provenance attestation.

```bash
# Debian, Ubuntu
sudo apt install ./lanleecher_<version>_amd64.deb
# Fedora, Rocky/RHEL
sudo dnf install ./lanleecher_<version>_amd64.rpm
# Arch Linux
sudo pacman -U lanleecher_<version>_amd64.pkg.tar.zst
# Alpine (no systemd unit is started there; run "lanleecher server" with your init system)
sudo apk add --allow-untrusted lanleecher_<version>_amd64.apk
```

Verify a download:

```bash
sha256sum -c SHA256SUMS --ignore-missing
gh attestation verify lanleecher_<version>_amd64.deb --repo lan-dot-party/LANLeecher
```

The package installs:

| Path | Content |
| --- | --- |
| `/usr/bin/lanleecher` | the binary |
| `/etc/lanleecher/config.yaml` | configuration (kept on updates) |
| `/etc/lanleecher/secret.key` | key that encrypts SNMP credentials, created on first install |
| `/var/lib/lanleecher/` | SQLite database and backups |
| `/usr/lib/systemd/system/lanleecher.service` | hardened systemd unit, runs as user `lanleecher` |

The service is **not** started automatically. First steps:

```bash
sudo nano /etc/lanleecher/config.yaml          # listen, public_url
sudo lanleecher user create admin --role admin  # asks for a password (min. 10 characters)
sudo systemctl enable --now lanleecher
```

Then open `public_url`, log in and continue with the [LAN party guide](lan-party-guide.md).

## Docker

```bash
docker compose up -d          # uses compose.yaml from the repository
docker compose exec lanleecher lanleecher user create admin --role admin
```

The image `ghcr.io/lan-dot-party/lanleecher` exists for amd64, arm64 and armv7. Data lives in the
volume `/var/lib/lanleecher`; mount your own configuration to `/etc/lanleecher/config.yaml`. Host
networking (as in `compose.yaml`) lets the resolvers reach clients via NetBIOS and mDNS and read the
host's neighbor table.

## From source

Go 1.24 or newer:

```bash
git clone https://github.com/lan-dot-party/LANLeecher.git
cd LANLeecher
CGO_ENABLED=0 go build -o bin/lanleecher ./cmd/lanleecher
./bin/lanleecher -c config.yaml config init     # writes config.yaml and secret.key next to it
./bin/lanleecher -c config.yaml user create admin --role admin
./bin/lanleecher -c config.yaml server
```

## Command line

| Command | Purpose |
| --- | --- |
| `lanleecher server` | run poller, web UI and API |
| `lanleecher config init [--force]` | write an example configuration and create `secret.key` |
| `lanleecher migrate` | apply database migrations (the server does this on start) |
| `lanleecher backup [file]` | consistent copy of the database while the server runs |
| `lanleecher user create <name> --role viewer\|operator\|admin [--language en\|de]` | create a user |
| `lanleecher user passwd <name>` | set a new password (ends the user's sessions) |
| `lanleecher user list` | list users |
| `lanleecher event start "<name>" [--notes …]` / `event stop` / `event list` | manage events |
| `lanleecher event schedule "<name>" --start YYYY-MM-DD --end YYYY-MM-DD [--start-time HH:MM] [--end-time HH:MM]` / `event schedules [--all]` / `event unschedule <id>` | plan events that start and stop by themselves (local time zone, last day inclusive) |
| `lanleecher switch add --name … --host … [--port 161] [--arp]` / `switch list` | add an SNMP v2c switch |
| `lanleecher version` | print the version |

All commands accept `-c/--config` (default `/etc/lanleecher/config.yaml`, or `LANLEECHER_CONFIG`).
Passwords are read from a prompt or, when piped, from stdin. `switch add` reads the community from
`LANLEECHER_SNMP_COMMUNITY` or stdin, never from an argument (arguments are visible in the process
list). Run as root, the CLI hands files it creates to the owner of the data directory, so
`sudo lanleecher …` is safe.

`systemctl reload lanleecher` (SIGHUP) reloads the switch list without a restart.

## Backup

```bash
sudo -u lanleecher lanleecher backup        # → /var/lib/lanleecher/backups/lanleecher-<date-time>.db
```

The backup uses `VACUUM INTO` and is consistent while the server runs. Keep `/etc/lanleecher/secret.key`
as well – without it the stored SNMP credentials cannot be decrypted.

## Upgrade

Install the new package. Configuration, key and database are kept; a running service is restarted
and applies database migrations on start. Take a backup first. To roll back, install the previous
package and restore the backup over `/var/lib/lanleecher/lanleecher.db` while the service is stopped.

## Uninstall

`apt remove lanleecher` (or the equivalent) keeps configuration and data; `apt purge lanleecher`
deletes `/etc/lanleecher`, `/var/lib/lanleecher` and the user.
