# Self-hosting

This guide takes you from an empty server to mailrelay receiving mail for your domain. If you just want to try it out, use the [quick start](../README.md#quick-start) instead. For Fly.io or a one-paste VPS setup, see [deploy platforms](deploy-platforms.md).

## Before you start

**You need inbound port 25.** Other mail servers only deliver to port 25, and many providers block it:

- Home ISPs often block it, and residential IPs are fine for receiving but may change.
- Some cloud providers block port 25 or ask you to request access first (AWS, Google Cloud and Azure restrict it; DigitalOcean blocks it on new accounts). Most of these restrictions are on *outbound* 25, which mailrelay doesn't need, but check your provider's docs and firewall.
- Small VPS providers (Hetzner, OVH, Vultr, a Raspberry Pi behind a port-forward) usually work.

Check from another machine once mailrelay is running:

```bash
nc -vz mx.example.com 25
```

**Can't open port 25?** Keep your existing mailbox and forward to mailrelay instead. You still need *some* host that accepts port 25, but it can be one cheap VPS shared by everything. See [Forwarding instead of MX](#forwarding-instead-of-mx).

**SMTP encryption is optional.** Configure [STARTTLS](#smtp-starttls) with a certificate for your MX hostname to encrypt mail from senders that support it.

## DNS

Pick a hostname for the server, for example `mx.example.com`, and the domain you want to receive mail for, for example `alerts.example.com`.

| Record | Name | Value |
| --- | --- | --- |
| `A` (and `AAAA` if you have IPv6) | `mx.example.com` | your server's IP |
| `MX` | `alerts.example.com` | `10 mx.example.com` |

Set `smtp.domain` to the hostname (`mx.example.com`). It's what mailrelay announces in its SMTP greeting.

> [!WARNING]
> An MX record takes over **all** mail for that name. Use a subdomain such as `alerts.example.com` unless you really mean to stop receiving normal mail on `example.com`.

Check it:

```bash
dig +short MX alerts.example.com
```

You don't need SPF, DKIM or a PTR record for mailrelay itself. Those matter for *sending* mail, and mailrelay only receives.

### Forwarding instead of MX

If your mail already lives somewhere else, set up a forwarding rule there that sends the mails you care about to an address mailrelay accepts:

- **Gmail:** Settings → Forwarding and POP/IMAP → add a forwarding address, then create a filter that forwards matching mail to it. Gmail sends a confirmation email to that address first. It shows up in the mailrelay UI as *Dropped*, so open it there to copy the code.
- **Fastmail:** Settings → Filters & Rules → new rule → "Send a copy to".
- **Cloudflare Email Routing:** add a custom address that routes to a destination address. You'll have to verify the destination the same way.

Forwarded mail keeps the original From/To headers but has a different envelope. mailrelay shows both, and rules can match either. If you set the auth checks to `enforce`, read [Auth checks and forwarded mail](troubleshooting.md#auth-checks-and-forwarded-mail) first.

## SMTP STARTTLS

Obtain a certificate for the MX hostname, for example with [Certbot](https://eff-certbot.readthedocs.io/en/stable/using.html). The standalone HTTP challenge needs inbound port 80 free; use a DNS challenge or an existing web server plugin if that is unsuitable:

```bash
sudo certbot certonly --standalone -d mx.example.com
sudo certbot renew --dry-run
```

Configure mailrelay with the full chain and private key:

```yaml
smtp:
  domain: "mx.example.com"
  tls_cert: "/etc/letsencrypt/live/mx.example.com/fullchain.pem"
  tls_key: "/etc/letsencrypt/live/mx.example.com/privkey.pem"
  require_tls: false
```

Set both paths or neither. With neither, SMTP stays plaintext. With both, mailrelay advertises STARTTLS and accepts TLS 1.2 or newer. Unreadable or invalid PEM files prevent startup. On every handshake it reads the pair again, following Certbot's renewed symlinks without a restart or SIGHUP. If a renewal leaves an unreadable or mismatched pair, new TLS handshakes fail until the files are fixed; existing connections continue.

The `mailrelay` user needs directory traversal and read access to the certificate and key, including the symlink targets under `/etc/letsencrypt/archive`. Grant access using a restricted group and key mode `0640`; preserve it on renewal with a Certbot deploy hook if needed. See [Certbot file permissions](https://eff-certbot.readthedocs.io/en/stable/using.html#where-are-my-certificates). In Docker, mount the whole `/etc/letsencrypt` tree read-only at the same path (both `live` and `archive` are needed), and grant the container user access to the key.

Leave `require_tls: false` for opportunistic delivery from the public internet. Setting it to `true` rejects `MAIL FROM` before STARTTLS with `530 5.7.0`; senders without TLS cannot deliver. STARTTLS alone does not prevent downgrade attacks. For supporting senders, [MTA-STS](https://www.rfc-editor.org/rfc/rfc8461) publishes an authenticated policy requiring TLS and a valid certificate for your MX hosts. Its DNS record and HTTPS policy must be hosted separately; mailrelay does not serve an MTA-STS policy.

Verify the certificate and handshake:

```bash
openssl s_client -starttls smtp -connect mx.example.com:25 -servername mx.example.com
SMTP_HOST=mx.example.com SMTP_PORT=25 SMTP_TLS=1 scripts/test-emails.sh
```

Email details show TLS or Plaintext beside the auth checks. Webhook payloads include `auth_result.tls`, and `mailrelay_smtp_sessions_total{transport="tls"}` counts TLS sessions.

## Install with Docker

Start from [`docker-compose.example.yaml`](../docker-compose.example.yaml):

```bash
mkdir mailrelay && cd mailrelay
curl -fsSL -o docker-compose.yaml https://raw.githubusercontent.com/gowthamgts/mailrelay/main/docker-compose.example.yaml
curl -fsSL -o config.yaml https://raw.githubusercontent.com/gowthamgts/mailrelay/main/config.example.yaml
```

Edit `config.yaml`. At minimum set:

- `smtp.domain` to your hostname
- `smtp.allowed_recipients` to the addresses you want, such as `"*@alerts.example.com"`. Without it mailrelay accepts mail for any address.

The image sets `http.addr`, `webui.db_path` and `webui.raw_email_dir` through environment variables, and env vars beat the config file. So those three keys in `config.yaml` are ignored in Docker; to change one, set its `MAILRELAY_` variable in the compose file instead. The UI listens on all interfaces inside the container, and the compose file publishes it only on the host's `127.0.0.1`.

Then uncomment the `./config.yaml:/data/config.yaml:ro` volume in `docker-compose.yaml`, and start it:

```bash
docker compose up -d
docker compose logs -f
```

Pin a version (`ghcr.io/gowthamgts/mailrelay:1.2.3`) instead of `latest` so upgrades happen when you choose. Releases are listed on the [releases page](https://github.com/gowthamgts/mailrelay/releases).

The container runs as a non-root `mailrelay` user and can still bind port 25, because Docker allows that inside the container by default.

## Install the binary

Release archives for Linux and macOS (amd64 and arm64) are on the [releases page](https://github.com/gowthamgts/mailrelay/releases).

```bash
tar xzf mailrelay_*_linux_amd64.tar.gz
sudo install mailrelay /usr/local/bin/
sudo useradd --system --home /var/lib/mailrelay --create-home mailrelay
sudo mkdir -p /etc/mailrelay
sudo cp config.example.yaml /etc/mailrelay/config.yaml
```

By default the database is created as `mailrelay.db` in the working directory. Point it at the data directory in `/etc/mailrelay/config.yaml`:

```yaml
webui:
  db_path: "/var/lib/mailrelay/mailrelay.db"
  raw_email_dir: "/var/lib/mailrelay/emails"
```

Leave `webui.enabled` on. Rules are stored in the web UI's database, so with it off mailrelay has no rules and drops every email.

Run it with systemd. `AmbientCapabilities` lets the unprivileged user bind port 25:

```ini
# /etc/systemd/system/mailrelay.service
[Unit]
Description=mailrelay
After=network-online.target
Wants=network-online.target

[Service]
User=mailrelay
Group=mailrelay
ExecStart=/usr/local/bin/mailrelay -config /etc/mailrelay/config.yaml
Restart=on-failure
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/mailrelay
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now mailrelay
journalctl -u mailrelay -f
```

## First checks

1. Open the UI. On a remote server without a [reverse proxy](reverse-proxy.md), use an SSH tunnel: `ssh -L 2623:127.0.0.1:2623 you@server`, then visit http://localhost:2623.
2. Send a test email from your normal mailbox to an allowed address. It should appear as *Dropped* (no rules yet).
3. Create a rule and click **Replay** on that email to try the webhook without sending another email.

Look at the startup logs too. mailrelay warns if `smtp.allowed_recipients` is empty or the web UI is reachable from other hosts without a login.

## Security checklist

- [ ] `smtp.allowed_recipients` lists only your addresses.
- [ ] The web UI is on `127.0.0.1`, or behind a [reverse proxy with a login](reverse-proxy.md).
- [ ] `webhook.allow_private_networks` is on only if your webhooks really point at your LAN, Docker or localhost. While it's on, anyone who can edit rules can make requests to your internal network.
- [ ] Your webhook receivers check a secret (see [Securing your receivers](webhooks.md#securing-your-receivers)).
- [ ] `smtp.max_message_bytes` suits your mail (default 25 MB).
- [ ] The auth checks are `log` until you've watched real mail pass, then `enforce` if you want to reject forgeries.
- [ ] Port 2623 isn't open in your firewall unless you mean it to be.

## Data, backups and upgrades

Everything lives in the data directory: `/data` in Docker, or wherever `webui.db_path` and `webui.raw_email_dir` point.

| Path | What it holds |
| --- | --- |
| `mailrelay.db` (plus `-wal` and `-shm`) | SQLite: rules, emails, delivery attempts, saved settings |
| `emails/<id>.eml` | Raw copy of each email, used by **Replay** and **Download** |
| `config.yaml` | Optional config file |

**Retention.** Emails, their delivery attempts and their `.eml` files are deleted after `webui.retention_days` (default 7), checked hourly. Rules and settings are never deleted. Disk use is roughly your daily mail volume times the retention days. If you start with `retention_days: 0` (keep forever), cleanup doesn't run until you restart with a value above 0.

**Saved settings win.** Once you click Save on the **Settings** page, the values stored there (log level, auth checks, retry and retention) override the config file and env vars on every start. Change them in the UI, not the config file.

**Backups.** The database is in WAL mode, so copying only `mailrelay.db` while it's running can give you a stale or broken copy. Use SQLite's online backup instead:

```bash
# Docker
docker run --rm -v mailrelay_data:/data -v "$PWD":/backup alpine \
  sh -c 'apk add -q sqlite && sqlite3 /data/mailrelay.db ".backup /backup/mailrelay.db"'

# binary
sqlite3 /var/lib/mailrelay/mailrelay.db ".backup /var/backups/mailrelay.db"
```

Back up `emails/` too if you want to be able to replay old mail. Rules are the part that's hard to recreate, and they're in the database.

**Upgrades.** Back up, change the image tag (or replace the binary), and restart. The database schema updates itself on startup. Read the release notes before jumping several versions.
