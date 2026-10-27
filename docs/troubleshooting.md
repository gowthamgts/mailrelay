# Troubleshooting

## What the statuses mean

| Status | Meaning |
| --- | --- |
| **Delivered** | Every matching rule's webhook returned 2xx. |
| **Dropped** | The email was accepted, but no rule matched it. Nothing was sent. |
| **Partial failure** | Some matching webhooks succeeded and some didn't. |
| **Failed** | Every matching webhook failed. Open the email to see each attempt's status code, error and response body. |
| **Rejected** | mailrelay refused the email at the SMTP level because an auth check set to `enforce` failed. The sender got a 550 error. |

Mail to an address not in `smtp.allowed_recipients` is also refused, but it never shows up in the UI. Look for `rejected recipient` in the logs, or the `mailrelay_smtp_recipients_rejected_total` metric.

## The email never arrived

Work through these in order:

1. **DNS.** `dig +short MX your-domain` should return your mailrelay host, and `dig +short A` on that host should return your server's IP.
2. **Port 25 is reachable.** From a machine outside your network: `nc -vz mx.example.com 25`. If it times out, check your firewall, your provider's port 25 policy and any port-forwarding. See [Before you start](self-hosting.md#before-you-start).
3. **The recipient is allowed.** Check the logs for `rejected recipient`. Patterns in `smtp.allowed_recipients` are globs matched against the whole address, such as `*@alerts.example.com`.
4. **The sender gave up or is retrying.** Most senders send you a bounce message saying why. Mail servers retry for hours, so a fix may still let a delayed email through.
5. **Send one yourself**, bypassing DNS, to check mailrelay on its own:

   ```bash
   swaks --server mx.example.com:25 --to you@alerts.example.com --from test@example.com
   ```

   For a batch of varied test emails (attachments, HTML, CC, forwarded mail), run [`scripts/test-emails.sh`](../scripts/test-emails.sh) with `SMTP_HOST` and `SMTP_PORT` set.

## The email shows as Dropped

No rule matched. Open the email and compare its fields with your rule's conditions:

- Header From/To and the SMTP envelope (`MAIL FROM` / `RCPT TO`) are separate fields. For forwarded mail they're often different. Match the one that has the address you expect.
- Globs ignore case and must match the whole value: `invoice` only matches a subject that is exactly "invoice", while `*invoice*` matches it anywhere. Regexes (`/…/`) are case-sensitive unless you add `(?i)`.
- Disabled rules don't match.

Fix the rule, then click **Replay** to try again without waiting for another email.

## The webhook failed

Open the email to see each attempt:

- **"private or internal" error:** the URL points at localhost, your LAN, a Docker network or Tailscale. Set `webhook.allow_private_networks: true` if that's intended.
- **4xx:** the receiver didn't like the request: a wrong URL, a missing auth header, or a payload it can't parse. 4xx responses aren't retried. Check the response body.
- **5xx or timeout:** the receiver is down or slow. These are retried; raise `retry.timeout` if the receiver needs longer.
- **Template errors** are flagged in the rule editor as you type. Remember that variables go in without quotes.

## Auth checks and forwarded mail

When a provider forwards mail, the forwarding server isn't one the original sender's SPF lists, so SPF fails, and DMARC can fail with it. mailrelay verifies ARC, which lets forwarders such as Gmail and Fastmail vouch for the original results. Not every forwarder adds ARC, though.

Before setting any check to `enforce`:

1. Leave all three on `log` (the default) for a while.
2. Look at the auth results on real emails in the UI, or at `mailrelay_auth_checks_total` in Prometheus.
3. Enforce only the checks that pass on the mail you care about.

You can change the auth modes on the **Settings** page without restarting.

## Settings I changed in the config file don't apply

Once you click Save on the **Settings** page, those values (log level, auth checks, retry and retention) are stored in the database and override the config file and env vars on every start. Change them on the Settings page instead.

In Docker, `http.addr`, `webui.enabled`, `webui.db_path` and `webui.raw_email_dir` are set by the image's env vars, which beat the config file. Set the matching `MAILRELAY_` variable instead.

## More detail in the logs

Set `log_level: debug` (or change it on the Settings page) for per-connection and per-rule logs. With Docker: `docker compose logs -f mailrelay`. With systemd: `journalctl -u mailrelay -f`.
