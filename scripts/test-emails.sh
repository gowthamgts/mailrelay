#!/usr/bin/env bash
set -euo pipefail

SMTP_HOST="${SMTP_HOST:-127.0.0.1}"
SMTP_PORT="${SMTP_PORT:-2525}"
DELAY="${DELAY:-0.3}"

if ! command -v swaks &>/dev/null; then
  echo "swaks is not installed."
  echo "  macOS:  brew install swaks"
  echo "  Linux:  apt install swaks / yum install swaks"
  exit 1
fi

COUNT=0

send() {
  local label="$1"; shift
  COUNT=$((COUNT + 1))
  printf "[%2d] %s\n" "$COUNT" "$label"
  swaks "$@" --server "${SMTP_HOST}:${SMTP_PORT}" --timeout 5 2>&1 | grep -E '^\*\*\*|<~|~>|<\*\*|=== ' || true
  echo
  sleep "$DELAY"
}

echo "=== mailrelay test emails ==="
echo "    target: ${SMTP_HOST}:${SMTP_PORT}"
echo

# ---------------------------------------------------------------------------
# PLAIN TEXT EMAILS
# ---------------------------------------------------------------------------

send "Plain text, minimal headers (alerts → delivered)" \
  --to alert@alerts.example.com \
  --from monitor@infra.example.com \
  --header "Subject: [test] plain text, minimal headers, alerts rule" \
  --body "Server web-01 CPU has been above 90% for 5 minutes."

send "Plain text, minimal headers (support → delivered)" \
  --to support@helpdesk.example.com \
  --from customer@acme.corp \
  --header "Subject: [test] plain text, minimal headers, support rule" \
  --body "I keep getting a 403 error when I try to login. Username: jdoe."

send "Plain text, minimal headers (no match → dropped)" \
  --to nobody@nowhere.test \
  --from spam@spammer.test \
  --header "Subject: [test] plain text, minimal headers, no match" \
  --body "Click here to claim your free iPad."

send "Plain text, minimal headers (broken webhook → failed)" \
  --to fail@broken.example.com \
  --from ops@internal.example.com \
  --header "Subject: [test] plain text, minimal headers, broken webhook" \
  --body "The webhook target does not exist."

# ---------------------------------------------------------------------------
# HTML-ONLY EMAILS
# ---------------------------------------------------------------------------

send "HTML-only body (alerts → delivered)" \
  --to oncall@alerts.example.com \
  --from cron@scheduler.example.com \
  --header "Subject: [test] html-only body, alerts rule" \
  --header "Content-Type: text/html; charset=UTF-8" \
  --body '<html><body><h2 style="color:red">Disk Alert</h2><p>Partition <code>/data</code> is at <strong>97%</strong> capacity.</p><ul><li>Host: db-03</li><li>Mount: /data</li><li>Free: 12 GB</li></ul></body></html>'

send "HTML-only body (support → delivered)" \
  --to support@company.io \
  --from vip@bigclient.com \
  --header "Subject: [test] html-only body, support rule" \
  --header "Content-Type: text/html; charset=UTF-8" \
  --body '<html><body><h1>Billing Problem</h1><p>I was charged <strong>twice</strong> for order <em>#12345</em>.</p><table border="1"><tr><th>Date</th><th>Amount</th></tr><tr><td>2026-02-28</td><td>$49.99</td></tr><tr><td>2026-02-28</td><td>$49.99</td></tr></table></body></html>'

send "HTML-only body (no match → dropped)" \
  --to news@updates.test \
  --from noreply@newsletter.test \
  --header "Subject: [test] html-only body, no match" \
  --header "Content-Type: text/html; charset=UTF-8" \
  --body '<html><body><h2>This Week in Tech</h2><ol><li>AI breakthroughs</li><li>New framework releases</li><li>Security advisories</li></ol><hr><p style="font-size:10px">Unsubscribe <a href="#">here</a></p></body></html>'

send "HTML-only body (broken webhook → failed)" \
  --to fail@test.example.com \
  --from deploy@ci.example.com \
  --header "Subject: [test] html-only body, broken webhook" \
  --header "Content-Type: text/html; charset=UTF-8" \
  --body '<html><body><p>Deployed <strong>v2.3.1</strong> to production.</p><pre>commit abc1234\nauthor: deploy-bot</pre></body></html>'

# ---------------------------------------------------------------------------
# MULTIPART EMAILS (text + HTML)
# ---------------------------------------------------------------------------

send "Multipart text+HTML (alerts → delivered)" \
  --to sre@alerts.example.com \
  --from nagios@monitoring.example.com \
  --header "Subject: [test] multipart text+html, alerts rule" \
  --header "MIME-Version: 1.0" \
  --body "CRITICAL: Replication lag on replica-02 exceeded 30s threshold." \
  --attach-type "text/html" \
  --attach-body '<html><body><h2 style="color:red">Replication Alert</h2><p>Replica <strong>replica-02</strong> lag: <span style="color:red;font-size:18px">47 seconds</span></p><p>Threshold: 30s</p></body></html>'

send "Multipart text+HTML (support → delivered)" \
  --to support@helpdesk.example.com \
  --from user@enterprise.com \
  --header "Subject: [test] multipart text+html, support rule" \
  --header "MIME-Version: 1.0" \
  --body "Hi, I'd love to see a dark mode option in the dashboard. It would reduce eye strain during night shifts. Thanks!" \
  --attach-type "text/html" \
  --attach-body '<html><body><p>Hi,</p><p>I'\''d love to see a <strong>dark mode</strong> option in the dashboard.</p><p>It would reduce eye strain during night shifts.</p><p>Thanks!</p></body></html>'

send "Multipart text+HTML (no match → dropped)" \
  --to promo@marketing.test \
  --from deals@store.test \
  --header "Subject: [test] multipart text+html, no match" \
  --header "MIME-Version: 1.0" \
  --body "SALE: 50% off all items. Use code SAVE50 at checkout." \
  --attach-type "text/html" \
  --attach-body '<html><body><div style="background:#ff6600;color:white;padding:20px;text-align:center"><h1>50% OFF</h1><p>Use code <strong>SAVE50</strong></p><a href="#" style="color:white">Shop Now</a></div></body></html>'

send "Multipart text+HTML (broken webhook → failed)" \
  --to fail@webhook.example.com \
  --from build@ci.example.com \
  --header "Subject: [test] multipart text+html, broken webhook" \
  --header "MIME-Version: 1.0" \
  --body "Build #1847 on branch main failed. 3 tests failed. See CI for details." \
  --attach-type "text/html" \
  --attach-body '<html><body><h3>Build #1847 Failed</h3><p>Branch: <code>main</code></p><p>Failed tests:</p><ul><li>TestAuth</li><li>TestWebhook</li><li>TestRetry</li></ul></body></html>'

# ---------------------------------------------------------------------------
# EMAILS WITH MULTIPLE / CUSTOM HEADERS
# ---------------------------------------------------------------------------

send "Multiple custom headers: X-Priority, X-Mailer, Reply-To (alerts → delivered)" \
  --to alert@alerts.example.com \
  --from pagerduty@infra.example.com \
  --header "Subject: [test] custom headers: priority+mailer+reply-to, alerts rule" \
  --header "X-Priority: 1" \
  --header "X-Mailer: PagerDuty/3.0" \
  --header "Reply-To: incidents@infra.example.com" \
  --header "X-Incident-ID: INC-20260301-001" \
  --header "Importance: high" \
  --body "Production is down in us-east-1. All services returning 503."

send "Multiple custom headers: List-* mailing list style (support → delivered)" \
  --to support@helpdesk.example.com \
  --from listserv@mailinglist.example.com \
  --header "Subject: [test] custom headers: list-id+list-unsubscribe, support rule" \
  --header "List-Id: <support-list.example.com>" \
  --header "List-Unsubscribe: <mailto:unsubscribe@example.com>" \
  --header "List-Archive: <https://lists.example.com/support>" \
  --header "Reply-To: support-list@example.com" \
  --header "X-Mailing-List: support-list" \
  --body "The postmortem for yesterday's outage has been published. Key takeaway: we need better circuit breakers."

send "Multiple custom headers: auto-reply indicators (no match → dropped)" \
  --to bounces@random.test \
  --from mailer-daemon@isp.test \
  --header "Subject: [test] custom headers: auto-submitted+precedence, no match" \
  --header "Auto-Submitted: auto-replied" \
  --header "X-Auto-Response-Suppress: All" \
  --header "Precedence: bulk" \
  --header "X-Mailer: Microsoft Outlook 16.0" \
  --body "I am currently out of the office with limited access to email."

send "Multiple custom headers: DKIM/SPF simulation (broken webhook → failed)" \
  --to fail@delivery.example.com \
  --from security@corp.example.com \
  --header "Subject: [test] custom headers: x-report-id+x-scan-type, broken webhook" \
  --header "X-Priority: 2" \
  --header "X-Mailer: SecurityScanner/1.0" \
  --header "X-Report-ID: RPT-2026-0301" \
  --header "X-Scan-Type: full" \
  --header "Organization: Corp Security Team" \
  --body "Weekly security scan completed. 0 critical, 2 high, 5 medium findings."

# ---------------------------------------------------------------------------
# EMAILS WITH CC RECIPIENTS
# ---------------------------------------------------------------------------

send "Email with CC (alerts → delivered)" \
  --to alert@alerts.example.com \
  --from ops@infra.example.com \
  --header "Subject: [test] cc header with multiple addresses, alerts rule" \
  --header "Cc: team-lead@infra.example.com, oncall@infra.example.com" \
  --body "cache-01 is under memory pressure. RSS at 14.2 GB / 16 GB limit."

send "Email with CC, HTML body (support → delivered)" \
  --to support@helpdesk.example.com \
  --from manager@bigclient.com \
  --header "Subject: [test] cc header + html-only body, support rule" \
  --header "Cc: cto@bigclient.com, account-mgr@company.io" \
  --header "Content-Type: text/html; charset=UTF-8" \
  --body '<html><body><p>Ticket <strong>#7823</strong> has been open for 5 days with no response.</p><p>Please escalate immediately. CC'\''ing our CTO and your account manager.</p></body></html>'

# ---------------------------------------------------------------------------
# EMAILS WITH MULTIPLE RECIPIENTS (envelope-level)
# ---------------------------------------------------------------------------

send "Multiple envelope recipients (alerts → delivered)" \
  --to alert@alerts.example.com \
  --to sre@alerts.example.com \
  --from monitor@infra.example.com \
  --header "Subject: [test] multiple envelope recipients, alerts rule" \
  --body "Automatic failover from us-east-1 to us-west-2 has been triggered."

# ---------------------------------------------------------------------------
# EDGE CASES: EMPTY / MINIMAL / SPECIAL
# ---------------------------------------------------------------------------

send "Near-empty body (alerts → delivered)" \
  --to alert@alerts.example.com \
  --from heartbeat@infra.example.com \
  --header "Subject: [test] near-empty body (single space), alerts rule" \
  --body " "

send "Very long subject line (support → delivered)" \
  --to support@helpdesk.example.com \
  --from verbose@client.com \
  --header "Subject: [test] very long subject line padding aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa end" \
  --body "See subject line for details. Invoice attached separately."

send "Unicode in subject and body (alerts → delivered)" \
  --to alert@alerts.example.com \
  --from intl@monitoring.example.com \
  --header "Subject: [test] unicode in subject: サーバー異常 très élevé überprüfen" \
  --body "Anomaly detected on サーバー tokyo-01. CPU: 98%. メモリ: 95%. Disk I/O: très élevé. Bitte sofort überprüfen."

send "Special characters in body (no match → dropped)" \
  --to test@random.test \
  --from dev@testing.test \
  --header "Subject: [test] special characters in body, no match" \
  --body 'Special chars: <angle> "quotes" & ampersand, backslash\\, single-quote'"'"', tabs	here, €£¥, em—dash, curly "quotes".'

send "Body with newlines and formatting (support → delivered)" \
  --to support@helpdesk.example.com \
  --from reporter@client.com \
  --header "Subject: [test] multi-line body with newlines, support rule" \
  --body "Steps to reproduce:
1. Go to /settings
2. Clear the 'Name' field
3. Click Save

Expected: validation error
Actual: 500 Internal Server Error

Browser: Chrome 120
OS: macOS 15.3"

# ---------------------------------------------------------------------------
# EMAILS WITH ATTACHMENT(S)
# ---------------------------------------------------------------------------

send "Plain text with small text attachment (alerts → delivered)" \
  --to alert@alerts.example.com \
  --from logwatch@infra.example.com \
  --header "Subject: [test] plain text + .log attachment, alerts rule" \
  --header "MIME-Version: 1.0" \
  --body "See attached error log summary for the past 24 hours." \
  --attach-type "text/plain" --attach-name "errors.log" \
  --attach-body '2026-03-01 00:12:33 ERROR connection timeout to db-02
2026-03-01 01:45:12 ERROR disk write failed /data/shard-3
2026-03-01 03:22:01 ERROR OOM killer invoked on worker-07'

send "HTML email with CSV attachment (support → delivered)" \
  --to support@helpdesk.example.com \
  --from accounting@client.com \
  --header "Subject: [test] html body + .csv attachment, support rule" \
  --header "Content-Type: text/html; charset=UTF-8" \
  --header "MIME-Version: 1.0" \
  --body '<html><body><p>Please find the discrepancy report attached.</p><p>We found <strong>3 overcharges</strong> totaling <em>$247.50</em>.</p></body></html>' \
  --attach-type "text/csv" --attach-name "discrepancies.csv" \
  --attach-body 'invoice_id,date,expected,charged,difference
INV-001,2026-01-15,49.99,99.99,50.00
INV-003,2026-02-01,29.99,129.99,100.00
INV-007,2026-02-15,0.00,97.50,97.50'

send "Multipart with JSON attachment (broken webhook → failed)" \
  --to fail@delivery.example.com \
  --from pipeline@ci.example.com \
  --header "Subject: [test] plain text + .json attachment, broken webhook" \
  --header "MIME-Version: 1.0" \
  --body "CI pipeline completed with failures. Test report attached." \
  --attach-type "application/json" --attach-name "test-report.json" \
  --attach-body '{"suite":"integration","total":142,"passed":139,"failed":3,"failures":["TestWebhookRetry","TestSMTPTimeout","TestMultiRecipient"]}'

# ---------------------------------------------------------------------------
# HTML EMAILS WITH RICH FORMATTING
# ---------------------------------------------------------------------------

send "Rich HTML: inline styles, tables, images (alerts → delivered)" \
  --to oncall@alerts.example.com \
  --from dashboard@monitoring.example.com \
  --header "Subject: [test] rich html: inline css + table + style tag, alerts rule" \
  --header "Content-Type: text/html; charset=UTF-8" \
  --body '<html><head><style>body{font-family:Arial,sans-serif}table{border-collapse:collapse;width:100%}th,td{border:1px solid #ddd;padding:8px;text-align:left}th{background:#4CAF50;color:white}.warn{color:#ff9800}.crit{color:#f44336}.ok{color:#4CAF50}</style></head><body><h2>Daily Infrastructure Report</h2><p>Generated: 2026-03-01 06:00 UTC</p><table><tr><th>Service</th><th>Status</th><th>Uptime</th><th>P99 Latency</th></tr><tr><td>API Gateway</td><td class="ok">OK</td><td>99.99%</td><td>45ms</td></tr><tr><td>Auth Service</td><td class="ok">OK</td><td>99.97%</td><td>120ms</td></tr><tr><td>Database</td><td class="warn">WARN</td><td>99.85%</td><td>250ms</td></tr><tr><td>Cache</td><td class="crit">CRITICAL</td><td>98.50%</td><td>890ms</td></tr></table><hr><p><small>This is an automated report. Do not reply.</small></p></body></html>'

send "Rich HTML: nested divs, links, code blocks (support → delivered)" \
  --to support@company.io \
  --from dev@enterprise.com \
  --header "Subject: [test] rich html: nested divs + pre + links, support rule" \
  --header "Content-Type: text/html; charset=UTF-8" \
  --body '<html><body><div style="max-width:600px;margin:0 auto;font-family:monospace"><h3>API Integration Issue</h3><p>We are getting a <code>429 Too Many Requests</code> response when calling:</p><pre style="background:#f4f4f4;padding:10px;border-radius:4px">POST /api/v2/webhooks
Host: api.company.io
Authorization: Bearer sk_live_...
Content-Type: application/json

{"url": "https://our-app.com/hook", "events": ["*"]}</pre><p>Our current rate: ~50 req/s. What is the limit?</p><p>Docs reference: <a href="https://docs.company.io/rate-limits">Rate Limits</a></p></div></body></html>'

# ---------------------------------------------------------------------------
# HEADER-ONLY VARIATIONS
# ---------------------------------------------------------------------------

send "Only Subject header, no other headers (no match → dropped)" \
  --to misc@unmatched.test \
  --from sender@origin.test \
  --header "Subject: [test] subject-only header, no match" \
  --body "This email has only a Subject header and a plain text body."

send "Many standard headers, plain text (alerts → delivered)" \
  --to alert@alerts.example.com \
  --from noc@infra.example.com \
  --header "Subject: [test] many standard headers: date+message-id+in-reply-to+references, alerts rule" \
  --header "Date: Sun, 01 Mar 2026 10:00:00 +0000" \
  --header "Message-ID: <maint-20260301@infra.example.com>" \
  --header "In-Reply-To: <thread-001@infra.example.com>" \
  --header "References: <thread-001@infra.example.com>" \
  --header "Reply-To: noc@infra.example.com" \
  --header "Organization: Infrastructure Team" \
  --header "X-Priority: 3" \
  --header "MIME-Version: 1.0" \
  --body "Maintenance window: 2026-03-02 02:00-04:00 UTC. Services affected: cache-01, cache-02."

send "Importance + sensitivity headers (support → delivered)" \
  --to support@helpdesk.example.com \
  --from legal@enterprise.com \
  --header "Subject: [test] importance+sensitivity+x-msmail-priority headers, support rule" \
  --header "Importance: high" \
  --header "Sensitivity: Company-Confidential" \
  --header "X-Priority: 1" \
  --header "X-MSMail-Priority: High" \
  --header "Reply-To: legal@enterprise.com" \
  --body "This is a confidential notification regarding a potential data breach. Please contact our legal team immediately."

# ---------------------------------------------------------------------------
# MULTIPART WITH BOTH TEXT+HTML AND ATTACHMENT
# ---------------------------------------------------------------------------

send "Full multipart: text + HTML + attachment (alerts → delivered)" \
  --to sre@alerts.example.com \
  --from reports@monitoring.example.com \
  --header "Subject: [test] full multipart: text + html + csv attachment, alerts rule" \
  --header "MIME-Version: 1.0" \
  --body "Weekly SLA report for 2026-W09. Overall SLA: 99.92%. See attached CSV for per-service breakdown." \
  --attach-type "text/html" \
  --attach-body '<html><body><h2>SLA Report: Week 09</h2><p>Overall: <strong style="color:green">99.92%</strong></p></body></html>' \
  --attach-type "text/csv" --attach-name "sla-w09.csv" \
  --attach-body 'service,uptime_pct,incidents
api-gateway,99.99,0
auth,99.97,1
database,99.85,2
cache,98.50,4'

send "Full multipart: text + HTML + attachment (broken webhook → failed)" \
  --to fail@delivery.example.com \
  --from ci@builds.example.com \
  --header "Subject: [test] full multipart: text + html + txt attachment, broken webhook" \
  --header "MIME-Version: 1.0" \
  --body "Release v3.0.0 is ready. Changelog attached." \
  --attach-type "text/html" \
  --attach-body '<html><body><h2>v3.0.0 Release Notes</h2><ul><li>New webhook retry logic</li><li>Web UI improvements</li><li>Bug fixes</li></ul></body></html>' \
  --attach-type "text/plain" --attach-name "CHANGELOG.txt" \
  --attach-body '## v3.0.0 (2026-03-01)
### Added
- Exponential backoff for webhook retries
- Email detail view in web UI
### Fixed
- Race condition in concurrent deliveries
- Memory leak in SMTP session handling'

echo "=== Done! Sent $COUNT test emails ==="
echo "    Open http://127.0.0.1:2623/ to view them."
