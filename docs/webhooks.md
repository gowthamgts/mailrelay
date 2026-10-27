# Webhook recipes

How payloads and templates work is covered in the [README](../README.md#payload). This page covers protecting the services that receive them, and ready-made templates for common destinations.

## What mailrelay sends

- Method `POST` by default (you can pick another per rule).
- `Content-Type: application/json`, unless you set your own in the rule's headers.
- `User-Agent: mailrelay/<version>`.
- Any headers you add in the rule's **Headers** box, one per line as `Key: Value`.

A request counts as delivered on any 2xx response. A 4xx response isn't retried, since sending the same payload again won't help. 5xx responses, connection errors and timeouts are retried with exponential backoff (`retry.*` in the config, or per rule).

## Securing your receivers

mailrelay doesn't sign requests, so anyone who learns your webhook URL can send fake ones. Give each receiver a secret it checks:

- **A secret header.** Add `Authorization: Bearer <token>` or `X-Webhook-Secret: <token>` in the rule's Headers box, and reject requests without it.
- **A secret in the URL.** Many services already work this way (Discord, Slack, Home Assistant and n8n webhook URLs contain a random ID). Treat those URLs as passwords.
- **HTTPS** for anything that leaves your network, so the secret and the email content aren't sent in the clear.

Don't rely on the `User-Agent`; anyone can copy it.

## Templates

Template variables are already JSON-encoded, so put them in **without quotes**. Each variable is one JSON value, so you can't join two of them into one string; use a separate field for each.

### ntfy

URL: `https://ntfy.sh/` (or your own server). Add `Authorization: Bearer <token>` if your topic is protected.

```
{"topic": "mail-alerts", "title": {{ .Subject }}, "message": {{ .TextBody | default .HTMLBody }}}
```

### Discord

URL: the channel's webhook URL (Channel settings → Integrations → Webhooks).

```
{"embeds": [{"title": {{ .Subject }}, "description": {{ .TextBody }}, "author": {"name": {{ .From }}}}]}
```

Discord rejects embed descriptions over 4096 characters. For long emails, send only the subject, or point the webhook at n8n first and trim it there.

### Slack

URL: an incoming webhook URL from a Slack app.

```
{"text": {{ .Subject }}}
```

### Home Assistant

URL: `https://homeassistant.local:8123/api/webhook/<webhook_id>`. No template is needed; the default payload works. In an automation with a webhook trigger, read the fields as `trigger.json.subject`, `trigger.json.from`, `trigger.json.text_body` and so on.

Home Assistant on your LAN has a private IP, so you also need `webhook.allow_private_networks: true`.

### n8n, Node-RED and other automation tools

Use a Webhook trigger node and leave the template empty. The full payload, including attachments as base64, is the easiest thing to work with there, and you can reshape it in the flow.

### Paperless-ngx

Paperless's upload endpoint wants a multipart file upload, which mailrelay can't send directly. Put n8n (or a small script) in between: receive the default payload, decode `attachments[].content` from base64, and post each file to `/api/documents/post_document/`.

## Testing

- The **Test** button in the rule editor checks that the URL is reachable.
- **Replay** on any email runs it through the current rules again, and the email's detail view shows each response body.
- Private and internal addresses (localhost, your LAN, Docker networks, Tailscale) are blocked unless `webhook.allow_private_networks` is on. A blocked request fails right away, without retries, and its error names the setting.
