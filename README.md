# Grafana Telegram Proxy

*Read in other languages: [Русский](README_RU.md).*

A lightweight, resilient, and secure Go-based proxy server for forwarding alerts from Grafana Alerting or Prometheus Alertmanager to the Telegram Bot API.

This service is designed specifically to bypass network restrictions and Telegram DC-level/ISP-level blocking. Equipped with a dynamic SOCKS5 proxy pool (supporting IPv4/IPv6, authentication, and rotation), a local pure-Go SQLite backup queue, and an exponential backoff retry worker, it guarantees the delivery of critical notifications even during complete network outages.

---

## Table of Contents
1. [Architecture & Alert Lifecycle](#architecture--alert-lifecycle)
2. [Configuration Parameters](#configuration-parameters)
3. [Installation & Startup](#installation--startup)
    - [Building from Source](#1-building-from-source)
    - [Running via Docker / Docker Compose](#2-running-via-docker-and-docker-compose)
    - [Setting up as a Systemd Service](#3-setting-up-as-a-systemd-service)
4. [Integrating with Grafana Alerting](#integrating-with-grafana-alerting)
    - [Option A: Webhook Contact Point (Recommended)](#option-a-webhook-contact-point-recommended)
    - [Option B: Grafana Alerting Provisioning Files](#option-b-grafana-alerting-provisioning-files)
    - [Option C: Prometheus Alertmanager](#option-c-prometheus-alertmanager)
5. [Monitoring & Metrics (Prometheus)](#monitoring--metrics-prometheus)
    - [Exported Metrics List](#exported-metrics-list)
    - [Prometheus Scrape Configuration Sample](#prometheus-scrape-configuration-sample-prometheusyml)
    - [Alert Rules for Self-Monitoring](#alert-rules-for-self-monitoring)
6. [Security & Log Management](#security--log-management)
7. [Diagnostics & Troubleshooting](#diagnostics--troubleshooting)

---

## Architecture & Alert Lifecycle

The service operates as a **transparent reverse proxy** for the Telegram Bot API. Instead of parsing and mapping complex internal JSON formats from Grafana, it accepts standard HTTP requests, routes them through a pool of SOCKS5 proxy servers, and returns the appropriate HTTP status.

```text
                               +-------------------------------------+
                               |        Grafana Telegram Proxy       |
                               |                                     |
[Grafana Alerting]             |  [HTTP Listener (/bot<token>/*)]    |
  │ (Telegram API format)      |                │                    |
  ▼                            |                ▼                    |
http://tg-proxy:8080/bot...    |         [Proxy Rotator]             |
                               |          ├── SOCKS5 Proxy 1         |
                               |          ├── SOCKS5 Proxy 2         |
                               |          └── Direct Fallback        |
                               |                │                    |
                               |          (On network error)         |
                               |                │                    |
                               |                ▼                    |
                               |         [SQLite Spool DB]           |
                               |                ▲                    |
                               |         (Background retries)        |
                               |                │                    |
                               |                ▼                    |
                               |       [Background Worker] ──────────┼──► [Telegram API]
                               +-------------------------------------+
```

### Request Flow:
1. **Request Reception**: The server listens on `PORT` and receives POST requests to `/bot<token>/sendMessage`.
2. **Proxy Rotation**: The request is routed via the first available SOCKS5 proxy in the pool.
    * If a proxy fails due to a network timeout or connection error, it is placed on a **60-second blacklist cooldown**.
    * The rotator immediately switches to the next proxy in the pool.
3. **Direct Fallback**: If all configured proxies are offline (or the pool is empty) and `DIRECT_FALLBACK` is enabled, the proxy attempts to connect directly to `api.telegram.org`.
4. **Spooling to SQLite**: If both the SOCKS5 proxies and direct connections fail:
    * The server extracts `chat_id` and `text` from the payload.
    * Saves these parameters, the bot token, and query params to the SQLite spool database (`alerts.db`).
    * Instantly returns an HTTP `202 Accepted` response with the body `{"ok":true,"description":"Alert accepted: spooled to local SQLite queue..."}` to Grafana. This prevents Grafana from continuously pounding the endpoint and shifts the delivery responsibility to the proxy.
5. **Background Retry Worker**:
    * A background worker scans the SQLite database every `RETRY_CHECK_INTERVAL`.
    * For each pending message, it uses an **exponential backoff** delay: `10s * 2^attempts` (capped at 1 hour).
    * When retrying, it appends chronological metadata to the text parameter showing the original send time and attempt count:
      `[⚠️ Retry Attempt #3 | Original Time: 2026-05-31 16:05:00 UTC]`
    * Once successfully delivered, the alert is deleted from the database.

---

## Configuration Parameters

Configure the service using environment variables:

| Environment Variable | Data Type | Default Value | Description |
| :--- | :--- | :--- | :--- |
| `PORT` | Integer | `8080` | Port for receiving requests from Grafana/Alertmanager. |
| `METRICS_PORT` | Integer | `9090` | Port for exposing Prometheus `/metrics`. |
| `DB_PATH` | String | `data/alerts.db` | Local path to the SQLite spool database file. |
| `PROXY_LIST_FILE` | String | `proxies.txt` | Path to the text file containing the SOCKS5 proxy list. |
| `PROXY_LIST_ENV` | String | `""` | Comma-separated list of SOCKS5 proxies (used if the file is missing/empty). |
| `RETRY_CHECK_INTERVAL`| Duration | `10s` | Frequency at which the background worker scans SQLite (e.g., `10s`, `1m`). |
| `DIRECT_FALLBACK` | Boolean | `true` | Enables direct connection fallback if all SOCKS5 proxies fail. |
| `LOG_LEVEL` | String | `info` | Minimum severity level to log (`debug`, `info`, `warn`, `error`). |
| `LOG_FORMAT` | String | `plain` | Log structure format (`plain` for terminal debugging, `json` for structured log aggregators like Loki). |
| `LOG_COLOR` | Boolean | `true` | Toggles colorized logs (plain format only). |

### SOCKS5 Proxy Format
Proxy addresses are read line-by-line from `PROXY_LIST_FILE` or parsed from `PROXY_LIST_ENV`. The `socks5://` scheme is prepended automatically if omitted.
- Without authentication: `socks5://192.168.1.100:1080`
- With authentication: `socks5://username:password@192.168.1.100:1080`
- IPv6 address: `socks5://username:password@[2001:db8::1]:1080`

---

## Installation & Startup

### 1. Building from Source
Go version 1.22+ is required.

1. Clone the project repository.
2. Compile a statically linked Go binary (with CGO disabled):
   ```bash
   CGO_ENABLED=0 go build -ldflags="-w -s" -o tg-proxy .
   ```
3. Create a `proxies.txt` file in the working directory and list your SOCKS5 proxies (one per line).
4. Run the service:
   ```bash
   PORT=8080 METRICS_PORT=9090 DB_PATH=./data/alerts.db ./tg-proxy
   ```

---

### 2. Running via Docker and Docker Compose
Because the Docker image is built from `scratch` (containing no operating system layers for size and vulnerability minimization), you must mount external directories for data storage and configurations.

#### Step A: Create Local Directories
Run in your working directory:
```bash
mkdir -p data config
touch config/proxies.txt
```
Populate `config/proxies.txt` with SOCKS5 proxies. If you do not wish to use proxies (spooling-and-retry only), leave the file empty.

#### Step B: Create docker-compose.yml
```yaml
version: '3.8'

services:
  tg-proxy:
    image: tg-proxy:latest
    build: .
    container_name: grafana-tg-proxy
    restart: always
    ports:
      - "8080:8080" # Alert receiving port
      - "9090:9090" # Prometheus metrics port
    environment:
      - PORT=8080
      - METRICS_PORT=9090
      - DB_PATH=/data/alerts.db
      - PROXY_LIST_FILE=/config/proxies.txt
      - RETRY_CHECK_INTERVAL=10s
      - DIRECT_FALLBACK=true
      - LOG_LEVEL=info
      - LOG_FORMAT=json
    volumes:
      - ./data:/data
      - ./config:/config
```

#### Step C: Start the Container
```bash
docker compose up -d --build
```

---

### 3. Setting up as a Systemd Service
To run the compiled binary as a Linux daemon:

1. Copy the binary to `/usr/local/bin/`:
   ```bash
   sudo cp tg-proxy /usr/local/bin/tg-proxy
   sudo chmod +x /usr/local/bin/tg-proxy
   ```
2. Create a system user:
   ```bash
   sudo useradd -r -s /bin/false tgproxy
   ```
3. Initialize the directories and configurations:
   ```bash
   sudo mkdir -p /var/lib/tg-proxy /etc/tg-proxy
   sudo touch /etc/tg-proxy/proxies.txt
   sudo chown -R tgproxy:tgproxy /var/lib/tg-proxy /etc/tg-proxy
   ```
4. Create the service definition `/etc/systemd/system/tg-proxy.service`:
   ```ini
   [Unit]
   Description=Grafana Telegram Alert Proxy Service
   After=network.target

   [Service]
   Type=simple
   User=tgproxy
   Group=tgproxy
   WorkingDirectory=/var/lib/tg-proxy
   Environment=PORT=8080
   Environment=METRICS_PORT=9090
   Environment=DB_PATH=/var/lib/tg-proxy/alerts.db
   Environment=PROXY_LIST_FILE=/etc/tg-proxy/proxies.txt
   Environment=RETRY_CHECK_INTERVAL=10s
   Environment=DIRECT_FALLBACK=true
   Environment=LOG_LEVEL=info
   Environment=LOG_FORMAT=plain
   Environment=LOG_COLOR=false
   ExecStart=/usr/local/bin/tg-proxy
   Restart=always
   RestartSec=5
   LimitNOFILE=65536

   [Install]
   WantedBy=multi-user.target
   ```
5. Reload systemd and start the service:
   ```bash
   sudo systemctl daemon-reload
   sudo systemctl enable tg-proxy
   sudo systemctl start tg-proxy
   ```
6. Check service status:
   ```bash
   sudo systemctl status tg-proxy
   ```

---

## Integrating with Grafana Alerting

### Option A: Webhook Contact Point (Recommended)
This method allows sending alerts directly via a standard Webhook integration in the Grafana UI. The proxy intercepts the Webhook payload, translates it into the format expected by Telegram, and forwards it.

#### Variant 1: Auto-Translation (Works on ANY Grafana version, including 9, 10, and 11)
If your Grafana version lacks the "Custom JSON payload" setting, the proxy handles translation automatically. It intercepts the default Grafana Webhook payload, extracts the `title` and `message` fields, and constructs a clean, styled HTML message.

Additionally, the proxy automatically:
* Escapes dangerous HTML characters (like `<`, `>`, `&`) to prevent Telegram parse errors.
* Translates Markdown formatting (such as `**bold**` into `<b>bold</b>` and `` `code` `` into `<code>code</code>`) into Telegram-supported HTML tags, ensuring the message arrives formatted.

1. In Grafana, navigate to **Alerting** -> **Contact points**.
2. Click **+ Add contact point**.
3. Select **Webhook** under **Integration**.
4. Set the **URL** to your proxy, **passing the `chat_id` as a query parameter**:
   `http://<PROXY_IP>:8080/bot<BOT_TOKEN>/sendMessage?chat_id=<CHAT_ID>`
   *(Example: `http://localhost:8080/bot1234567:ABC/sendMessage?chat_id=-1001234567890`)*
5. Set **HTTP Method** to `POST`.
6. Leave the rest of the **Optional Webhook settings** empty (or configure the default title/message templates if you wish to override what Grafana generates).
7. Click **Test** and save the contact point.

---

#### Variant 2: Custom Payload Formatting (Grafana 12+ only)
If you are on Grafana 12+ and want to define the JSON structure directly in Grafana's UI:

1. Select **Webhook** integration.
2. Set the **URL** to:
   `http://<PROXY_IP>:8080/bot<BOT_TOKEN>/sendMessage`
3. Under **Optional Webhook settings**:
   - Add header: `Content-Type: application/json`
   - Enable the **Custom Payload** toggle.
   - Enter your templated JSON payload:
     ```json
     {
       "chat_id": "-1001234567890",
       "parse_mode": "HTML",
       "text": "🚨 <b>[{{ .Status | toUpper }}] Grafana Alerts</b>\n\n{{ range .Alerts }}• <b>{{ .Labels.alertname }}</b>: {{ .Annotations.summary }}\n{{ end }}"
     }
     ```
4. Test and save the contact point.

---

### Option B: Grafana Alerting Provisioning Files
To configure your contact points as code (IaC), add the webhook definition in your `/etc/grafana/provisioning/alerting/` YAML files:

```yaml
apiVersion: 1
contactPoints:
  - orgId: 1
    name: "Telegram Proxy Webhook"
    receivers:
      - uid: "tg_proxy_webhook_001"
        type: "webhook"
        settings:
          url: "http://tg-proxy.monitoring.svc:8080/bot123456789:ABC-DEF1234ghIkl-zyx987w/sendMessage?chat_id=-1001234567890"
          httpMethod: "POST"
          singleEmail: false
          customHeaders:
            Content-Type: "application/json"
```

---

### Option C: Prometheus Alertmanager
If routing alerts via Prometheus Alertmanager, use the built-in `telegram_configs` and override the `api_url` setting:

```yaml
global:
  resolve_timeout: 5m

receivers:
  - name: 'telegram-receiver'
    telegram_configs:
      - bot_token: '123456789:ABC-DEF1234ghIkl-zyx987w'
        chat_id: -1001234567890
        api_url: 'http://<PROXY_IP>:8080' # Proxy automatically appends /bot<token>/sendMessage
        parse_mode: 'HTML'
        message: |
          🚨 <b>[{{ .Status | toUpper }}] Alertmanager Notification</b>
          {{ range .Alerts }}
          • <b>{{ .Labels.alertname }}</b>: {{ .Annotations.summary }}
          {{ end }}
```

---

## Monitoring & Metrics (Prometheus)

The server exposes Prometheus-compatible metrics on `METRICS_PORT` (default `9090`) at the `/metrics` path.

### Exported Metrics List:

| Metric Name | Type | Labels | Description |
| :--- | :--- | :--- | :--- |
| `tg_proxy_alerts_received_total` | Counter | *none* | Total alerts received from Grafana/Alertmanager. |
| `tg_proxy_alerts_delivered_total` | Counter | `route` (`socks5`, `direct`), `type` (`initial`, `retry`) | Total successfully delivered Telegram alerts. |
| `tg_proxy_alerts_failed_total` | Counter | `reason` (e.g. `network_error`, `http_4xx`, `spooled`) | Total failed delivery attempts with details. |
| `tg_proxy_proxy_health_status` | Gauge | `proxy` (connection string) | Connectivity status of each proxy (1 = Healthy, 0 = In cooldown). |
| `tg_proxy_queued_alerts_count` | Gauge | *none* | Number of alerts currently spooled in the SQLite queue. |

---

### Prometheus Scrape Configuration Sample (`prometheus.yml`):
```yaml
scrape_configs:
  - job_name: 'tg-proxy-metrics'
    static_configs:
      - targets: ['tg-proxy.monitoring.svc:9090']
```

---

### Alert Rules for Self-Monitoring:
You can load these rules in Prometheus to monitor the state of the proxy:

```yaml
groups:
  - name: tg-proxy-self-monitoring
    rules:
      - alert: TelegramProxyQueueFillingUp
        expr: tg_proxy_queued_alerts_count > 20
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Telegram Proxy queue is filling up"
          description: "Over 20 alerts are queued in SQLite. All proxies might be blocked or network connectivity is completely down."

      - alert: TelegramProxyAllProxiesOffline
        expr: sum(tg_proxy_proxy_health_status) == 0 and count(tg_proxy_proxy_health_status) > 0
        for: 2m
        labels:
          severity: critical
        annotations:
          summary: "All SOCKS5 proxies are offline"
          description: "All SOCKS5 proxies in the pool are unhealthy. Sending is going via direct fallback (if enabled) or spooled to SQLite."
```

---

## Security & Log Management

The service includes security hardening to prevent credential leakage:

1. **Credential Masking**: Logs automatically redact sensitive elements:
   - **Telegram Bot Tokens**: Substrings matching `/bot[0-9]+:[A-Za-z0-9_-]+/` are replaced with `/bot****:****/`.
   - **SOCKS5 Credentials**: Usernames and passwords (e.g. `socks5://admin:secretPass@192.168.1.1:1080`) are masked as `socks5://****:****@192.168.1.1:1080` in configuration logs and connection warnings.
2. **Log Structure Formats**:
   - `plain`: Color-coded text suited for local development.
   - `json`: Recommended for production (Loki/Elasticsearch compatible):
     ```json
     {"time":"2026-05-31T17:45:10Z","level":"info","scope":"http","message":"Received Telegram request: POST /bot****:****/sendMessage from client 127.0.0.1:49281"}
     ```

---

## Diagnostics & Troubleshooting

### Local Test using Curl
Test the proxy directly using a `curl` POST request:
```bash
curl -X POST http://localhost:8080/bot<YOUR_BOT_TOKEN>/sendMessage \
  -H "Content-Type: application/json" \
  -d '{"chat_id": "<CHAT_ID>", "text": "🔔 Test message from proxy"}'
```

* **Direct Delivery Response**: `{"ok":true,"result":{...}}`
* **Network Error / Spooled Response**: `{"ok":true,"description":"Alert accepted: spooled to local SQLite queue..."}`

### Inspecting SQLite Queue
If messages are stuck in the queue, open the SQLite database to query the spool table:
```bash
sqlite3 data/alerts.db
```
```sql
SELECT id, chat_id, attempts, original_time, next_retry, status FROM pending_alerts;
```
To exit the sqlite client: `.exit`

### Common Errors:

#### 1. `502 Bad Gateway` on alert delivery
- **Cause**: The proxy failed to route the request through both the SOCKS5 pool and direct fallback, and spooling to SQLite also failed (often due to write permission issues in the `data/` directory).
- **Solution**: Check permissions on the `data/` directory. Ensure the user running the process has write permissions.

#### 2. `database is locked` error logs
- **Cause**: Another database connection is locking the SQLite database.
- **Solution**: The service operates SQLite in WAL mode with connection limits restricted to 1 (`MaxOpenConns=1`). Do not keep concurrent write transactions open externally on `alerts.db` while the proxy is running.

#### 3. Duplicate messages received in Telegram
- **Cause**: The connection broke after Telegram received and sent the message, but before the proxy received the HTTP response. The proxy left the message in SQLite for retry, resulting in a duplicate.
- **Solution**: This is normal for *At-Least-Once* delivery systems. If it occurs frequently, increase the timeout when using slow SOCKS5 proxies.
