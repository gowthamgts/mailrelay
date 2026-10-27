# Deploy platforms

mailrelay needs one thing most app platforms don't offer: **inbound TCP on port 25**. Other mail servers only deliver there, so a platform that only routes HTTP, or gives you a random TCP port, can't receive mail.

| Platform | Works? | Why |
| --- | --- | --- |
| [Fly.io](#flyio) | Yes | TCP services on any port, with a dedicated IPv4 (about $2/month) |
| [Any VPS](#any-vps-with-cloud-init) (Hetzner, DigitalOcean Droplets, Vultr, Linode, ...) | Yes | You get a real IP; paste the cloud-init file when creating the server |
| Render | No | Only ports 80 and 443 are open to the internet |
| Railway | No | The TCP proxy picks a random public port, and inbound SMTP isn't allowed |
| Heroku, Vercel, Netlify, DigitalOcean App Platform, Cloud Run | No | HTTP only |

If you're stuck on an HTTP-only platform, run mailrelay on a small VPS or Fly instead, and keep your webhook receivers wherever they are.

## Fly.io

Files: [`deploy/fly/fly.toml`](../deploy/fly/fly.toml) and [`deploy/fly/config.yaml`](../deploy/fly/config.yaml).

You need the [`fly` CLI](https://fly.io/docs/flyctl/install/) and a Fly account with a payment method (dedicated IPv4 addresses are paid).

**1. Get the files** into an empty folder:

```bash
mkdir mailrelay-fly && cd mailrelay-fly
curl -fsSLO https://raw.githubusercontent.com/gowthamgts/mailrelay/main/deploy/fly/fly.toml
curl -fsSLO https://raw.githubusercontent.com/gowthamgts/mailrelay/main/deploy/fly/config.yaml
```

**2. Edit `config.yaml`:** set `smtp.domain` to your mail hostname and `smtp.allowed_recipients` to your addresses.

**3. Create the app** without deploying yet. Pick a name and a region when asked:

```bash
fly launch --copy-config --no-deploy --ha=false
```

**4. Allocate a dedicated IPv4.** Plain SMTP has no hostname for Fly's shared proxy to route on, so port 25 only works on an IP of your own:

```bash
fly ips allocate-v4
```

**5. Deploy:**

```bash
fly deploy --ha=false
```

`--ha=false` keeps it to one Machine. Rules and emails are in SQLite on a single volume, so a second Machine would have its own empty database.

**6. Point DNS at it.** `fly ips list` shows the addresses. Create an `A` record (and `AAAA` for the IPv6) for your mail hostname, then the `MX` record, as described in [Self-hosting → DNS](self-hosting.md#dns).

**7. Open the web UI.** It isn't published to the internet. Tunnel to it:

```bash
fly proxy 2623:2623
```

and visit http://localhost:2623. The startup log warns that the UI listens on a non-localhost address; inside the Machine that's expected, because only `fly proxy` and your private Fly network can reach it. To publish the UI, first set up [OIDC login](reverse-proxy.md#oidc-login) in `config.yaml`, then add an `[http_service]` with `internal_port = 2623` to `fly.toml` and set `redirect_url` to your app's URL.

**Updating:** `fly deploy --ha=false` pulls the latest image. Pin a version by changing `[build] image` to `ghcr.io/gowthamgts/mailrelay:<version>`. Config changes need a redeploy too, since `config.yaml` is copied in at deploy time.

**Backups:** `fly volumes snapshots list` shows Fly's daily volume snapshots. For your own copy, see [Data, backups and upgrades](self-hosting.md#data-backups-and-upgrades).

## Any VPS with cloud-init

File: [`deploy/cloud-init.yaml`](../deploy/cloud-init.yaml).

Most VPS providers let you paste a cloud-init script ("user data") when you create a server. This one installs Docker and starts mailrelay with the same setup as [`docker-compose.example.yaml`](../docker-compose.example.yaml).

1. Copy `cloud-init.yaml` and edit the two lines marked `EDIT`: your mail hostname and the addresses to accept.
2. Create an Ubuntu 24.04 server and paste the file into the user data / cloud-init field (Hetzner: "Cloud config"; DigitalOcean: "Advanced options → Add initialization scripts"; Vultr: "Cloud-Init User-Data").
3. Point DNS at the server's IP ([Self-hosting → DNS](self-hosting.md#dns)).
4. Open the UI over SSH: `ssh -L 2623:127.0.0.1:2623 root@<server-ip>`, then visit http://localhost:2623.

Setup takes a minute or two after the server boots. Check progress with `cloud-init status --wait`, and see mailrelay's logs with `docker compose -f /opt/mailrelay/docker-compose.yaml logs -f`. To change the config later, edit `/opt/mailrelay/config.yaml` and run `docker compose -f /opt/mailrelay/docker-compose.yaml restart`.

Make sure the provider's firewall (Hetzner Firewalls, DigitalOcean Cloud Firewalls, AWS security groups, ...) allows inbound TCP 25, and see [Before you start](self-hosting.md#before-you-start) for providers that restrict port 25.
