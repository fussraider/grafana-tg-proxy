# Grafana Telegram Proxy

*Читать на других языках: [English](README.md).*

Легковесный, отказоустойчивый и безопасный прокси-сервер на Go для пересылки оповещений из Grafana Alerting или Prometheus Alertmanager в Telegram Bot API.

Этот сервис разработан специально для обхода сетевых ограничений и блокировок Telegram на уровне дата-центров (DC-level blocks) или интернет-провайдеров. Благодаря поддержке динамического пула SOCKS5-прокси, локальной очереди в SQLite и фоновой отсылке с экспоненциальной задержкой, он гарантирует доставку важных оповещений даже во время сетевых сбоев.

---

## Содержание
1. [Архитектура и жизненный цикл алертов](#архитектура-и-жизненный-цикл-алертов)
2. [Параметры конфигурации](#параметры-конфигурации)
3. [Установка и запуск сервиса](#установка-и-запуск-сервиса)
    - [Сборка из исходников](#1-сборка-из-исходных-файлов)
    - [Запуск через Docker / Docker Compose](#2-запуск-через-docker-и-docker-compose)
    - [Настройка службы Systemd](#3-настройка-как-системной-службы-systemd)
4. [Настройка интеграции с Grafana](#настройка-интеграции-с-grafana)
    - [Способ 1: Webhook Contact Point в Grafana (Рекомендуемый)](#способ-1-webhook-contact-point-в-grafana-рекомендуемый)
    - [Способ 2: Provisioning-файлы Grafana Alerting](#способ-2-provisioning-файлы-grafana-alerting)
    - [Способ 3: Prometheus Alertmanager](#способ-3-prometheus-alertmanager)
5. [Мониторинг и метрики (Prometheus)](#мониторинг-и-метрики-prometheus)
    - [Список экспортируемых метрик](#список-экспортируемых-метрик)
    - [Пример конфигурации сбора Prometheus](#пример-конфигурации-сбора-prometheus-prometheusyml)
    - [Правила оповещений для контроля самого прокси](#правила-оповещений-alert-rules-для-контроля-самого-прокси)
6. [Безопасность и логирование](#безопасность-и-логирование)
7. [Диагностика и тестирование (Troubleshooting)](#диагностика-и-тестирование-troubleshooting)

---

## Архитектура и жизненный цикл алертов

Сервис функционирует как **прозрачный реверс-прокси** для Telegram API. Вместо разбора структуры сложных внутренних JSON-форматов Grafana, он принимает стандартные HTTPS-запросы к Telegram Bot API, маршрутизирует их через пул прокси-серверов и возвращает соответствующий HTTP-статус.

```text
                               +-------------------------------------+
                               |        Grafana Telegram Proxy       |
                               |                                     |
[Grafana Alerting]             |  [HTTP Listener (/bot<token>/*)]    |
  │ (Формат Telegram API)      |                │                    |
  ▼                            |                ▼                    |
http://tg-proxy:8080/bot...    |         [Proxy Rotator]             |
                               |          ├── SOCKS5 Прокси 1        |
                               |          ├── SOCKS5 Прокси 2        |
                               |          └── Direct Fallback        |
                               |                │                    |
                               |          (При сбое сети)            |
                               |                │                    |
                               |                ▼                    |
                               |         [SQLite Spool DB]           |
                               |                ▲                    |
                               |         (Фоновые попытки)           |
                               |                │                    |
                               |                ▼                    |
                               |       [Background Worker] ──────────┼──► [Telegram API]
                               +-------------------------------------+
```

### Схема обработки запроса:
1. **Прием запроса**: Сервер слушает порт `PORT` и принимает запросы на `/bot<token>/sendMessage`.
2. **Ротация прокси**: Запрос отправляется через первый доступный SOCKS5-прокси из пула.
    * Если прокси выдает сетевую ошибку или таймаут, он автоматически помещается в **black-list на 60 секунд**.
    * Сервис немедленно переключается на следующий прокси в пуле.
3. **Прямое соединение (Direct Fallback)**: Если все настроенные прокси-серверы временно недоступны (или список пуст), а опция `DIRECT_FALLBACK` включена, совершается попытка отправить запрос напрямую на `api.telegram.org`.
4. **Сохранение в очередь (Spooling)**: Если и прокси, и прямое соединение завершились сбоем:
    * Сервер извлекает из тела запроса `chat_id` и `text`.
    * Сохраняет эти параметры, токен и параметры запроса в SQLite (файл `alerts.db`).
    * Клиенту (Grafana) сразу возвращается успешный HTTP-код `202 Accepted` с телом `{"ok":true,"description":"Alert accepted: spooled to local SQLite queue..."}`. Это предотвращает повторные агрессивные отправки со стороны самой Grafana, перекладывая надежность на прокси.
5. **Фоновый ретраер (Retry Worker)**:
    * Фоновый поток раз в `RETRY_CHECK_INTERVAL` считывает записи из SQLite.
    * Для каждого повторно отправляемого сообщения применяется **экспоненциальная задержка** (backoff): `10s * 2^attempts` (максимум до 1 часа).
    * При переотправке к тексту сообщения автоматически приписывается мета-информация о времени первой отправки и количестве попыток, например:
      `[⚠️ Retry Attempt #3 | Original Time: 2026-05-31 16:05:00 UTC]`
    * После успешной отправки сообщение удаляется из БД.

---

## Параметры конфигурации

Настройка сервиса выполняется через переменные окружения. Ниже приведена таблица всех поддерживаемых параметров:

| Переменная окружения | Тип данных | По умолчанию | Описание |
| :--- | :--- | :--- | :--- |
| `PORT` | Integer | `8080` | Порт приема запросов от Grafana/Alertmanager. |
| `METRICS_PORT` | Integer | `9090` | Порт экспорта метрик Prometheus `/metrics`. |
| `DB_PATH` | String | `data/alerts.db` | Локальный путь к базе данных SQLite очереди недоставленных сообщений. |
| `PROXY_LIST_FILE` | String | `proxies.txt` | Путь к текстовому файлу со списком прокси. |
| `PROXY_LIST_ENV` | String | `""` | Список прокси через запятую (применяется, если файл пуст или отсутствует). |
| `RETRY_CHECK_INTERVAL`| Duration | `10s` | Частота сканирования базы данных SQLite фоновым воркером (например, `10s`, `1m`). |
| `DIRECT_FALLBACK` | Boolean | `true` | Разрешает попытку прямой отправки без прокси в случае недоступности всех SOCKS5. |
| `LOG_LEVEL` | String | `info` | Детализация логов (`debug`, `info`, `warn`, `error`). |
| `LOG_FORMAT` | String | `plain` | Формат вывода логов (`plain` — форматированный текст для консоли, `json` — для систем сбора логов вроде Loki). |
| `LOG_COLOR` | Boolean | `true` | Включает цветные теги для plain-формата логов. |

### Формат списка прокси
Адреса прокси-серверов считываются либо построчно из файла, указанного в `PROXY_LIST_FILE`, либо из переменной `PROXY_LIST_ENV` через запятую. Схема `socks5://` подставляется автоматически, если она опущена.

Примеры валидных конфигураций прокси:
- IPv4 без авторизации: `socks5://109.201.10.33:1080`
- IPv4 с авторизацией: `socks5://user:password@109.201.10.33:1080`
- IPv6 адрес: `socks5://user:password@[2001:db8::1]:1080`

---

## Установка и запуск сервиса

### 1. Сборка из исходных файлов
Для сборки требуется компилятор Go версии 1.22+.

1. Склонируйте репозиторий с проектом.
2. Скомпилируйте статически слинкованный бинарный файл без внешних зависимостей (CGO выключен):
   ```bash
   CGO_ENABLED=0 go build -ldflags="-w -s" -o tg-proxy .
   ```
3. Создайте в директории с файлом текстовый файл `proxies.txt` и внесите туда ваши SOCKS5 прокси (по одному адресу в строке).
4. Запустите сервис:
   ```bash
   PORT=8080 METRICS_PORT=9090 DB_PATH=./data/alerts.db ./tg-proxy
   ```

---

### 2. Запуск через Docker и Docker Compose
Так как Docker-образ собран на базе пустого окружения `scratch`, в контейнере нет файловой системы. Поэтому необходимо примонтировать внешние папки для сохранения базы данных SQLite и чтения конфигурации прокси.

#### Шаг A: Создание локальной структуры папок
Выполните команды в вашей рабочей директории:
```bash
mkdir -p data config
touch config/proxies.txt
```
Запишите SOCKS5-прокси в файл `config/proxies.txt`. Если прокси не используются (работа только в режиме spooling-retry), оставьте файл пустым.

#### Шаг Б: Создание docker-compose.yml
```yaml
version: '3.8'

services:
  tg-proxy:
    image: tg-proxy:latest
    build: .
    container_name: grafana-tg-proxy
    restart: always
    ports:
      - "8080:8080" # Прием алертов
      - "9090:9090" # Метрики Prometheus
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

#### Шаг В: Запуск контейнера
```bash
docker compose up -d --build
```

---

### 3. Настройка как системной службы Systemd
Если вы запускаете скомпилированный бинарный файл непосредственно на хосте Linux:

1. Скопируйте собранный бинарник `tg-proxy` в `/usr/local/bin/`:
   ```bash
   sudo cp tg-proxy /usr/local/bin/tg-proxy
   sudo chmod +x /usr/local/bin/tg-proxy
   ```
2. Создайте системного пользователя для запуска службы:
   ```bash
   sudo useradd -r -s /bin/false tgproxy
   ```
3. Подготовьте папки для базы данных и конфигурации:
   ```bash
   sudo mkdir -p /var/lib/tg-proxy /etc/tg-proxy
   sudo touch /etc/tg-proxy/proxies.txt
   sudo chown -R tgproxy:tgproxy /var/lib/tg-proxy /etc/tg-proxy
   ```
4. Создайте файл конфигурации службы `/etc/systemd/system/tg-proxy.service`:
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
5. Перезагрузите демоны systemd и запустите службу:
   ```bash
   sudo systemctl daemon-reload
   sudo systemctl enable tg-proxy
   sudo systemctl start tg-proxy
   ```
6. Проверьте статус службы:
   ```bash
   sudo systemctl status tg-proxy
   ```

---

## Настройка интеграции с Grafana

Поскольку Grafana не поддерживает нативное изменение адреса Telegram API через пользовательский интерфейс, используются обходные пути.

### Способ 1: Webhook Contact Point в Grafana (Рекомендуемый)
Этот метод позволяет легко перенаправить алерты через UI Grafana. Вебхук отправляет запрос на прокси-сервер, который автоматически подменяет формат на понятный для Telegram и отправляет его получателю.

#### Вариант А: Автотрансляция (Работает на ЛЮБЫХ версиях Grafana, включая 9, 10, 11)
Если в вашей версии Grafana нет настроек кастомного JSON-тела (Custom JSON payload), прокси-сервер сделает всё автоматически. Он перехватит стандартное уведомление Grafana, распарсит его поля `title` и `message` и сформирует из них красивое Telegram-сообщение.

При этом прокси-сервер автоматически:
* Экранирует опасные спецсимволы HTML (такие как `<`, `>`, `&`) для защиты от ошибок разметки Telegram.
* Преобразует базовую разметку Markdown (например, выделение `**жирным**` превратит в `<b>жирным</b>`, а `` `код` `` — в `<code>код</code>`) в поддерживаемый Telegram HTML-формат, благодаря чему сообщение придет с правильным стилем вместо сырого Markdown-текста.

1. Откройте интерфейс Grafana, перейдите в меню **Alerting** -> **Contact points**.
2. Нажмите **+ Add contact point**.
3. Выберите в выпадающем списке **Integration** пункт **Webhook**.
4. В поле **URL** укажите адрес запущенного прокси-сервера, **обязательно добавив `chat_id` получателя как query-параметр**:
   `http://<IP_ВАШЕГО_СЕРВИСА>:8080/bot<ВАШ_BOT_TOKEN>/sendMessage?chat_id=<ID_ЧАТА_ИЛИ_КАНАЛА>`
   *(Например: `http://localhost:8080/bot1234567:ABC/sendMessage?chat_id=-1001234567890`)*
5. В поле **HTTP Method** выберите `POST`.
6. Остальные настройки в блоке **Optional Webhook settings** (такие как Title, Message и т.д.) можно оставить пустыми или настроить шаблоны заголовка и текста оповещения, которые Grafana будет пересылать по умолчанию.
7. Нажмите **Test** в правом верхнем углу, чтобы проверить доставку. Нажмите **Save contact point**.

---

#### Вариант Б: Ручная настройка payload (Только для Grafana 12+)
Если вы используете Grafana 12+ и хотите полностью переопределить JSON-тело запроса, вы можете сделать это в интерфейсе:

1. Выберите тип интеграции **Webhook**.
2. В поле **URL** укажите адрес:
   `http://<IP_ВАШЕГО_СЕРВИСА>:8080/bot<ВАШ_BOT_TOKEN>/sendMessage`
3. В меню **Optional Webhook settings**:
   - Нажмите **Add header**: `Content-Type: application/json`
   - Переведите переключатель **Custom Payload** (Кастомный JSON) в активный режим.
   - Вставьте JSON-шаблон сообщения:
     ```json
     {
       "chat_id": "-1001234567890",
       "parse_mode": "HTML",
       "text": "🚨 <b>[{{ .Status | toUpper }}] Алерты Grafana</b>\n\n{{ range .Alerts }}• <b>{{ .Labels.alertname }}</b>: {{ .Annotations.summary }}\n{{ end }}"
     }
     ```
4. Нажмите **Test** и сохраните контактную точку.

---

### Способ 2: Provisioning-файлы Grafana Alerting
Если вы настраиваете инфраструктуру как код (IaC), вы можете описать контактную точку типа `webhook` в YAML-файлах инициализации Grafana (`/etc/grafana/provisioning/alerting/`):

```yaml
apiVersion: 1
contactPoints:
  - orgId: 1
    name: "Telegram Proxy Webhook"
    receivers:
      - uid: "tg_proxy_webhook_001"
        type: "webhook"
        settings:
          url: "http://tg-proxy.monitoring.svc:8080/bot123456789:ABC-DEF1234ghIkl-zyx987w/sendMessage"
          httpMethod: "POST"
          singleEmail: false # для вебхуков означает отправку единого тела вместо массива
          customHeaders:
            Content-Type: "application/json"
          # Шаблон тела запроса
          body: |
            {
              "chat_id": "-1001234567890",
              "parse_mode": "HTML",
              "text": "🚨 <b>[{{ .Status | toUpper }}] Алерты Grafana</b>\n\n{{ range .Alerts }}• <b>{{ .Labels.alertname }}</b>: {{ .Annotations.summary }}\n{{ end }}"
            }
```

---

### Способ 3: Prometheus Alertmanager
Если вы хотите пересылать алерты из стандартного Alertmanager в обход ограничений:
Alertmanager нативно поддерживает переопределение базового API эндпоинта для Telegram с помощью поля `api_url`. В файле `alertmanager.yml` добавьте следующую конфигурацию:

```yaml
global:
  resolve_timeout: 5m

receivers:
  - name: 'telegram-receiver'
    telegram_configs:
      - bot_token: '123456789:ABC-DEF1234ghIkl-zyx987w'
        chat_id: -1001234567890
        api_url: 'http://<IP_ВАШЕГО_СЕРВИСА>:8080' # Прокси-сервер автоматически добавит /bot<token>/sendMessage
        parse_mode: 'HTML'
        message: |
          🚨 <b>[{{ .Status | toUpper }}] Оповещение Alertmanager</b>
          {{ range .Alerts }}
          • <b>{{ .Labels.alertname }}</b>: {{ .Annotations.summary }}
          {{ end }}
```

---

## Мониторинг и метрики (Prometheus)

Сервер экспортирует телеметрию в формате Prometheus на выделенном порту `METRICS_PORT` (по умолчанию `9090`) по пути `/metrics`.

### Список экспортируемых метрик:

| Имя метрики | Тип метрики | Теги (Labels) | Описание |
| :--- | :--- | :--- | :--- |
| `tg_proxy_alerts_received_total` | Counter | *нет* | Общее число входящих запросов от Grafana/Alertmanager. |
| `tg_proxy_alerts_delivered_total` | Counter | `route` (`socks5`, `direct`), `type` (`initial`, `retry`) | Успешно отправленные сообщения в Telegram. |
| `tg_proxy_alerts_failed_total` | Counter | `reason` (например, `network_error`, `http_4xx`, `spooled`) | Количество попыток отправки, завершившихся неудачно. |
| `tg_proxy_proxy_health_status` | Gauge | `proxy` (строка подключения) | Текущий статус здоровья прокси-серверов (1 - Здоров, 0 - В черном списке/Сбой). |
| `tg_proxy_queued_alerts_count` | Gauge | *нет* | Текущий объем неотправленных сообщений в локальной SQLite БД. |

---

### Пример конфигурации сбора Prometheus (`prometheus.yml`):
Добавьте в конфигурацию Prometheus следующий `scrape_job`:

```yaml
scrape_configs:
  - job_name: 'tg-proxy-metrics'
    static_configs:
      - targets: ['tg-proxy.monitoring.svc:9090']
```

---

### Правила оповещений (Alert Rules) для контроля самого прокси:
Вы можете настроить правила алертинга в Prometheus для отслеживания состояния самого сервиса проксирования.

```yaml
groups:
  - name: tg-proxy-self-monitoring
    rules:
      # Оповещение, если очередь недоставленных сообщений растет
      - alert: TelegramProxyQueueFillingUp
        expr: tg_proxy_queued_alerts_count > 20
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Очередь сообщений Telegram Proxy переполняется"
          description: "В SQLite БД прокси-сервера скопилось более 20 сообщений. Возможно, все прокси-серверы заблокированы или отсутствует подключение к сети."

      # Оповещение, если все настроенные SOCKS5-прокси мертвы
      - alert: TelegramProxyAllProxiesOffline
        expr: sum(tg_proxy_proxy_health_status) == 0 and count(tg_proxy_proxy_health_status) > 0
        for: 2m
        labels:
          severity: critical
        annotations:
          summary: "Все SOCKS5 прокси-серверы оффлайн"
          description: "Пул SOCKS5-прокси полностью недоступен. Отправка сообщений идет напрямую (если включено) или сохраняется в БД."
```

---

## Безопасность и логирование

Сервис уделяет повышенное внимание конфиденциальности передаваемых данных:

1. **Маскирование логов**: Любой вывод логов (уровни `INFO`, `DEBUG` или `ERROR`) автоматически перехватывается и очищается от конфиденциальных данных:
   - **Токены Telegram-ботов**: Строки вида `/bot1234567:AAHdf834jdfhJDFH83/sendMessage` автоматически маскируются под `/bot****:****/sendMessage`.
   - **Авторизационные данные SOCKS5**: Логины и пароли в адресах прокси-серверов пула (например, `socks5://admin:secretPass@192.168.1.1:1080` при выводе загруженного списка прокси или ошибок подключения) автоматически заменяются маской `socks5://****:****@192.168.1.1:1080`.
2. **Проверка доступности (пинг) прокси при старте**: При запуске сервис производит проверку связи со всеми настроенными SOCKS5-прокси, совершая пробное подключение до серверов Telegram (`api.telegram.org:443`). В логах выводится статус каждого прокси и время задержки (пинг) в миллисекундах. Если прокси недоступен, выводится предупреждение (`WARN`), а прокси временно исключается из ротации на 60 секунд.
3. **Форматы вывода логов**:
   - `plain`: Подходит для разработки (с цветовым кодированием компонентов).
   - `json`: Рекомендуется для production. Позволяет парсить логи напрямую в Grafana Loki без написания сложных регулярных выражений:
     ```json
     {"time":"2026-05-31T17:45:10Z","level":"INFO","scope":"http","message":"Received Telegram request: POST /bot****:****/sendMessage from client 127.0.0.1:49281"}
     ```

---

## Диагностика и тестирование (Troubleshooting)

### Быстрый тест работоспособности через curl
Вы можете сымитировать отправку алерта из Grafana с помощью обычной утилиты командной строки `curl`.

Выполните команду на сервере, где запущен прокси:
```bash
curl -X POST http://localhost:8080/bot<ВАШ_BOT_TOKEN>/sendMessage \
  -H "Content-Type: application/json" \
  -d '{"chat_id": "<ID_ВАШЕГО_ЧАТА>", "text": "🔔 Тестовое сообщение через прокси-сервер"}'
```

* Ожидаемый ответ при работающей сети/прокси: `{"ok":true,"result":{...}}`
* Ожидаемый ответ при сбое сети/прокси (активация spooling):
  `{"ok":true,"description":"Alert accepted: spooled to local SQLite queue for retry due to transmission failure."}`

### Проверка базы данных очереди SQLite
Если сообщения задерживаются в очереди, вы можете проверить файл базы данных SQLite, чтобы увидеть список сохраненных записей.

1. Откройте базу данных через консольный клиент:
   ```bash
   sqlite3 data/alerts.db
   ```
2. Выполните запрос для просмотра количества попыток и времени создания:
   ```sql
   SELECT id, chat_id, attempts, original_time, next_retry, status FROM pending_alerts;
   ```
3. Выход из консоли: `.exit`

### Частые ошибки и методы их решения:

#### 1. Ошибка `502 Bad Gateway` при отправке алерта
- **Причина**: Прокси-сервер не смог отправить сообщение через SOCKS5-прокси, а прямое подключение отключено (`DIRECT_FALLBACK=false`) или тоже заблокировано, и при этом запись в SQLite БД не удалась (например, из-за прав доступа на файл).
- **Решение**: Проверьте права на запись в папку `data/`. Убедитесь, что пользователь, под которым запущен процесс, имеет права создавать файлы в указанной директории.

#### 2. Сообщения логов выводят `Failed to spool alert to SQLite: ... database is locked`
- **Причина**: База данных SQLite заблокирована другим процессом или множественными конкурентными транзакциями.
- **Решение**: В архитектуре сервиса применен пул с ограничением на 1 подключение (`db.SetMaxOpenConns(1)`), работающий в режиме WAL. Убедитесь, что к файлу `alerts.db` не обращаются сторонние утилиты чтения/записи в монопольном режиме во время работы прокси.

#### 3. В Telegram приходят дубликаты сообщений
- **Причина**: Telegram принял сообщение и отправил его получателю, но сетевой сокет разорвался до того, как прокси успел получить HTTP-ответ `200 OK` от Telegram API. Прокси счел транзакцию неудавшейся и оставил сообщение в очереди на повторную отправку.
- **Решение**: Это стандартное поведение гарантированной доставки по принципу At-Least-Once (минимум один раз). Увеличьте таймаут ожидания прокси, если используете медленные мобильные или перегруженные публичные SOCKS5 прокси.
