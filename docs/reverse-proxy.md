# Reverse proxy and login

The web UI can read every email and edit every rule, so it shouldn't be reachable by anyone else. You have three options:

1. **Keep it on localhost** and reach it over an SSH tunnel or a VPN such as Tailscale. This is the default and needs nothing else.
2. **Put it behind a reverse proxy** that adds HTTPS, and turn on mailrelay's built-in [OIDC login](#oidc-login).
3. **Put it behind a reverse proxy that handles login itself** (Authelia, Authentik's proxy outpost, oauth2-proxy, Cloudflare Access, Tailscale Serve) and leave mailrelay's login off.

The reverse proxy only handles the web UI on port 2623. SMTP on port 25 goes straight to mailrelay. Don't put it behind an HTTP proxy.

## Proxy examples

Each example serves the UI at `https://mail.example.com`, with mailrelay listening on `127.0.0.1:2623` (or `mailrelay:2623` on a shared Docker network). Keep the `Host` header as it is; the login callback URL is built from it.

### Caddy

```caddyfile
mail.example.com {
	reverse_proxy 127.0.0.1:2623
}
```

Caddy gets the certificate itself and passes everything mailrelay needs.

### nginx

```nginx
server {
    listen 443 ssl;
    server_name mail.example.com;
    # ssl_certificate / ssl_certificate_key ...

    client_max_body_size 1m;

    location / {
        proxy_pass http://127.0.0.1:2623;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_read_timeout 1h; # keeps the live-update stream (/api/events) open
    }
}
```

The live-update stream sends `X-Accel-Buffering: no`, so nginx doesn't buffer it.

### Traefik

Add labels to the `mailrelay` service in your compose file, and put it on the same network as Traefik:

```yaml
labels:
  - traefik.enable=true
  - traefik.http.routers.mailrelay.rule=Host(`mail.example.com`)
  - traefik.http.routers.mailrelay.entrypoints=websecure
  - traefik.http.routers.mailrelay.tls.certresolver=letsencrypt
  - traefik.http.services.mailrelay.loadbalancer.server.port=2623
```

Keep the published `127.0.0.1:2623:2623` port only if you still want SSH-tunnel access.

## OIDC login

mailrelay can sign users in with any OpenID Connect provider: Google, Microsoft Entra ID, Okta, Auth0, Keycloak, Authentik, Pocket ID, Kanidm and so on. GitHub isn't supported because it doesn't implement OIDC.

Login settings take a list, so they go in the config file, not env vars.

**1. Register an app with your provider.** Create an OAuth/OIDC client (often called a "web application" or "confidential client") and set its redirect URI to:

```
https://mail.example.com/auth/callback
```

**2. Configure mailrelay:**

```yaml
http:
  auth:
    enabled: true
    redirect_url: "https://mail.example.com/auth/callback"
    session_secret: "a-long-random-string" # openssl rand -hex 32
    allowed_email_domains: ["example.com"]
    providers:
      - name: "authentik"
        issuer_url: "https://auth.example.com/application/o/mailrelay/"
        client_id: "..."
        client_secret: "..."
```

| Setting | Why it matters |
| --- | --- |
| `redirect_url` | Must match the redirect URI you registered, exactly. If you leave it empty mailrelay builds it from the request's `Host` header, always with `https` unless the host is `localhost`. Setting it avoids surprises behind proxies. |
| `session_secret` | Signs the session cookie. Without it a random key is made at each start, so everyone is logged out on restart. |
| `allowed_email_domains` | Who may sign in. When it's empty **any account at the provider can log in**, which with Google means any Google account. When set, the provider must also report the email as verified. |
| `cookie_secure` | The session cookie is marked Secure (HTTPS only) unless `http.addr` is on localhost. In Docker `http.addr` is `0.0.0.0`, so logins only work over HTTPS. If you really serve the UI over plain HTTP on a LAN, set `cookie_secure: false`. |
| `http.protect_metrics` | `/metrics` stays public when login is on, so Prometheus can scrape it. Set `true` to require a login for it too. `/healthz` is always public. |

mailrelay discovers the provider's endpoints from `issuer_url`, and fails to start if it can't reach it within 30 seconds. For `name: "google"` you can leave `issuer_url` out.

**Provider notes:**

- **Google:** Google Cloud Console → APIs & Services → Credentials → OAuth client ID → Web application. Always set `allowed_email_domains`.
- **Authentik:** create an OAuth2/OpenID provider and an application. The issuer URL ends in `/application/o/<app-slug>/`.
- **Keycloak:** create a client with "Client authentication" on. The issuer is `https://<host>/realms/<realm>`.
- **Pocket ID:** create an OIDC client. The issuer is the base URL of your Pocket ID instance.

If login fails, check the logs. A `login denied` line shows the email and whether the provider marked it verified. A redirect URI error on the provider's page means `redirect_url` doesn't match what you registered.
