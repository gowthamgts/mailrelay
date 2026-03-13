# Prometheus Metrics

mailrelay exposes Prometheus metrics at `/metrics` on the HTTP server (default `127.0.0.1:2623`). All metrics use the `mailrelay_` prefix.

## SMTP

| Metric                                             | Type      | Labels  | Description                                        |
| -------------------------------------------------- | --------- | ------- | -------------------------------------------------- |
| `mailrelay_smtp_connections_total`                 | counter   | —       | SMTP connections accepted                          |
| `mailrelay_smtp_emails_received_total`             | counter   | —       | Emails successfully received                       |
| `mailrelay_smtp_recipients_rejected_total`         | counter   | —       | Recipients rejected by `allowed_recipients` filter |
| `mailrelay_smtp_email_size_bytes`                  | histogram | —       | Size of received emails in bytes                   |
| `mailrelay_smtp_email_processing_duration_seconds` | histogram | —       | Time spent processing emails (auth + parsing)      |
| `mailrelay_smtp_email_errors_total`                | counter   | `stage` | Email processing errors by stage                   |

## Authentication

| Metric                                      | Type    | Labels            | Description                                                 |
| ------------------------------------------- | ------- | ----------------- | ----------------------------------------------------------- |
| `mailrelay_auth_checks_total`               | counter | `check`, `result` | Auth checks performed (SPF/DKIM/DMARC/ARC × pass/fail/none) |
| `mailrelay_auth_enforcement_failures_total` | counter | `check`           | Emails rejected due to auth enforcement                     |

## Rule Engine

| Metric                            | Type    | Labels | Description                      |
| --------------------------------- | ------- | ------ | -------------------------------- |
| `mailrelay_rules_matched_total`   | counter | `rule` | Times each rule matched an email |
| `mailrelay_rules_evaluated_total` | counter | —      | Emails evaluated against rules   |
| `mailrelay_rules_no_match_total`  | counter | —      | Emails that matched no rules     |

## Webhooks

| Metric                               | Type      | Labels           | Description                  |
| ------------------------------------ | --------- | ---------------- | ---------------------------- |
| `mailrelay_webhook_dispatches_total` | counter   | `rule`, `status` | Webhook dispatch outcomes    |
| `mailrelay_webhook_duration_seconds` | histogram | `rule`           | Webhook HTTP request latency |
| `mailrelay_webhook_retries_total`    | counter   | `rule`           | Webhook retry attempts       |
| `mailrelay_webhook_in_flight`        | gauge     | —                | Webhooks currently in flight |

## Scrape config

```yaml
# prometheus.yml
scrape_configs:
  - job_name: mailrelay
    static_configs:
      - targets: ["127.0.0.1:2623"]
```
