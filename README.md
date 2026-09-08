# MeetNote — Telegram-помощник для конспектирования встреч

MeetNote принимает голосовые сообщения, аудио и тестовые текстовые файлы в Telegram, создаёт фоновую задачу, получает транскрипцию и выжимку, сохраняет материалы в PostgreSQL и позволяет искать информацию по встречам.

Speech- и LLM-клиенты по умолчанию работают в полностью автономном mock-режиме: для запуска и защиты не нужны внешние AI API. Токен требуется только для Telegram-бота.

## Возможности

- Telegram-команды /start, /load, /list, /status, /get, /find, /chat, /retry;
- обработка Telegram text, voice, audio и document;
- автоматическая регистрация по неизменяемому Telegram user ID;
- запрет работы в групповых чатах, чтобы не публиковать материалы встреч;
- потоковое скачивание файлов через Telegram Bot API с лимитом 20 МБ;
- durable inbox: update сохраняется в PostgreSQL до подтверждения Telegram;
- идемпотентность upload/chat/retry по Telegram update_id;
- четыре параллельных обработчика команд и не более одной активной команды на пользователя;
- fail-fast проверка не позволяет выставить небезопасное число goroutine, DB connections или слишком короткий lease;
- длинная транскрипция из /get отправляется одним .txt-файлом;
- отдельные процессы bot и worker: один long-polling bot и масштабируемые workers;
- PostgreSQL-очередь с FOR UPDATE SKIP LOCKED, lease-токенами и heartbeat;
- независимое периодическое восстановление задач с просроченным lease;
- статусы created → processing → transcribed → summarized → completed и failed;
- транзакционное сохранение результатов и истории статусов;
- user-scoped SQL для списка, статуса, транскрипции, поиска, чата и retry;
- JSON-логи без токена и содержимого встреч;
- unit-, HTTP contract-, concurrency- и PostgreSQL integration-тесты.
- Docker healthcheck для bot/worker и PostgreSQL; порт PostgreSQL на host привязан только к loopback.

## Быстрый запуск

### 1. Создайте Telegram-бота

Откройте @BotFather, выполните /newbot и получите токен.

### 2. Настройте окружение

~~~bash
cp .env.example .env
~~~

Заполните в .env:

~~~dotenv
MEETNOTE_TELEGRAM_TOKEN=123456789:ваш_токен
~~~

Реальный токен нельзя добавлять в Git.

### 3. Запустите приложение

~~~bash
docker compose up -d --build
docker compose logs -f bot worker
~~~

Compose запускает:

- postgres — базу данных;
- bot — единственную long-polling реплику Telegram;
- worker — ограниченный pool фоновой обработки.

Для увеличения пропускной способности обработки можно масштабировать только workers:

~~~bash
docker compose up -d --scale worker=2
~~~

Несколько long-polling bot-реплик с одним токеном запускать не нужно.

## Работа в Telegram

Отправьте боту:

- /start — регистрация;
- /load — подсказка по загрузке встречи;
- голосовое сообщение или аудиофайл — создание встречи;
- .txt или .md — тестовая транскрипция;
- /list — список встреч;
- /status 1 — статус, даты, выжимка или ошибка;
- /get 1 — полная транскрипция;
- /find релиз — поиск только по своим встречам;
- /chat Кто отвечает за релиз? — ответ по последним завершённым встречам;
- /retry 1 — повторная обработка только failed-встречи.

Загрузка завершается сразу после сохранения файла и транзакционного создания встречи с задачей. Speech/LLM выполняются процессом worker, поэтому Telegram handler не ждёт долгую обработку.

## Локальный запуск

~~~bash
make db-up
~~~

В первом терминале:

~~~bash
export MEETNOTE_TELEGRAM_TOKEN='ваш токен'
make bot
~~~

Во втором терминале:

~~~bash
make worker
~~~

Служебные режимы бинарника:

| Режим | Назначение |
|---|---|
| meetnote bot | Telegram long polling |
| meetnote worker | Фоновая обработка |
| meetnote migrate | Применение миграций PostgreSQL |
| meetnote health | Проверка PostgreSQL |

Пользовательские команды через аргументы бинарника отсутствуют: пользовательский интерфейс проекта — Telegram.

## Архитектура

~~~mermaid
flowchart LR
    TG["Telegram Bot API"] --> POLL["Long polling"]
    POLL --> INBOX["PostgreSQL durable inbox"]
    INBOX --> TA["Bounded update pool (4)"]
    TA --> APP["Application service"]
    APP --> RP["Repository port"]
    APP --> FS["FileStore port"]
    APP --> LP["LLMClient port"]
    WORKER["Bounded worker pool"] --> TP["TaskRepository port"]
    WORKER --> SP["SpeechClient port"]
    WORKER --> LP
    RP --> PG["PostgreSQL"]
    TP --> PG
    FS --> DISK["Shared upload volume"]
    SP --> SMOCK["Mock Speech"]
    LP --> LMOCK["Mock LLM"]
~~~

Ключевые каталоги:

- cmd/meetnote — composition root, режимы процессов и graceful shutdown;
- internal/telegram — Bot API client, long polling, команды и форматирование;
- internal/inbox — transport-neutral port для durable delivery и lease;
- internal/app — пользовательские use cases и outbound-интерфейсы;
- internal/domain — сущности, статусы и доменные ошибки;
- internal/postgres — repository, очередь и migration runner;
- internal/storage — потоковое локальное файловое хранилище с изоляцией через os.Root;
- internal/clients/mock — автономные Speech/LLM реализации;
- internal/worker — ограниченный pool, lease heartbeat и recovery.

Telegram adapter зависит от интерфейса application use cases. internal/app не импортирует Telegram, HTTP, pgx или конкретные AI-клиенты.

Основные библиотеки:

- pgx для PostgreSQL и пула соединений;
- goose для версионирования схемы;
- caarlos0/env для типизированной загрузки переменных окружения;
- стандартный log/slog для структурированных JSON-логов;
- x/sync/errgroup для совместной отмены и ожидания фоновых процессов;
- testify для проверок и обязательных условий в тестах.

## Миграции

Миграции выполняются библиотекой goose и встраиваются в бинарник. Каждое изменение схемы хранится в одном SQL-файле с двумя секциями:

~~~sql
-- +goose Up
-- Применение изменения.

-- +goose Down
-- Откат изменения.
~~~

В проекте четыре последовательные миграции:

| Файл | Назначение |
|---|---|
| 00001_create_meeting_storage.sql | Пользователи, встречи, задачи, транскрипции, выжимки, история статусов, чат и основные индексы |
| 00002_add_processing_task_leases.sql | Lease-поля задач и индекс поиска просроченной обработки |
| 00003_add_request_idempotency_and_telegram_inbox.sql | Ключи идемпотентности, проверки содержимого и надёжная очередь Telegram updates |
| 00004_add_user_quotas_and_processing_leases.sql | Учёт размера файлов и ограничение одной активной задачи на пользователя |

Миграции выполняются транзакционно. PostgreSQL advisory lock не позволяет двум репликам одновременно изменять схему. Для ручного отката можно использовать команду goose down с каталогом internal/postgres/migrations.

## Загрузка файла

1. Handler берёт file_id из voice, audio или document.
2. getFile возвращает временный file_path.
3. Файл потоково скачивается с context timeout.
4. FileStore проверяет расширение и фактический размер потока.
5. Application service создаёт встречу и задачу одной DB-транзакцией.
6. Worker асинхронно выполняет Speech → Summary → Completed.

Telegram Bot API позволяет боту скачать файл размером не более 20 МБ, поэтому MEETNOTE_MAX_FILE_MB по умолчанию равен 20 и не может быть выше в bot-режиме.

## Лимиты и защита от монополизации

Лимиты выбраны под транспорт Telegram и учебное приложение, а не скопированы с web-сервисов. Telegram через getFile разрешает боту скачать не более 20 МБ. Для ориентира Fireflies разрешает до 150 минут на одну загруженную запись, а Otter принимает web-импорты до 5 ГБ; такие размеры неприменимы к Telegram Bot API.

| Ограничение | Значение | Зачем |
|---|---:|---|
| Файл | 20 МБ | жёсткий предел Telegram getFile |
| Встречи одного пользователя | 100 | защита от спама маленькими или пустыми файлами |
| Незавершённые встречи одного пользователя | 10 | один автор не создаёт неограниченный backlog |
| Хранение одного пользователя | 200 МБ | ограничение диска; до 10 файлов Telegram-максимума |
| История чата одного пользователя | 1000 ответов | старые ответы удаляются и не увеличивают базу без ограничений |
| Одновременные Telegram update | 4 | ограниченная нагрузка на сеть, память и PostgreSQL |
| Одновременные update одного пользователя | 1 | один пользователь не занимает весь pool |
| Попытки update | 8 | временные ошибки повторяются, постоянные не зацикливаются |
| Хранение обработанных update | 7 дней | durable inbox не растёт бесконечно |
| Транскрипция | 500 000 символов | запас для многочасовой встречи, ограничение БД/LLM |
| Выжимка | 12 000 символов | максимум три обычных Telegram-сообщения |
| Ответ chat | 8 000 символов | максимум два обычных Telegram-сообщения |
| Вопрос / поиск | 2 000 / 500 символов | ограничение входа для LLM и поискового SQL |
| Inline /get | 12 000 символов | более длинный результат отправляется .txt-документом |

Источники для сравнения: [Telegram Bot API](https://core.telegram.org/bots/api), [Fireflies upload limits](https://guide.fireflies.ai/articles/3893959957-learn-about-the-uploads-feature-in-fireflies), [Otter import limits](https://help.otter.ai/hc/en-us/articles/360047733574-Import-an-audio-or-video-file).

## Пользователи и безопасность

- backend формирует user_id как telegram:<from.id>;
- пользователь не может передать чужой ID параметром команды;
- бот отвечает только в private chats;
- каждый пользовательский SQL-запрос связывает meetings → users и фильтрует владельца;
- чужой и несуществующий meeting ID возвращают одинаковый ErrNotFound;
- токен не включается в structured logs и тексты сетевых ошибок.

## Очередь и несколько worker-реплик

Claim выполняется транзакционно через FOR UPDATE SKIP LOCKED. При claim выдаются уникальный lease token и locked_until. Отдельный processing_user_leases допускает не более одной активной Speech/LLM-задачи на пользователя даже при нескольких worker-репликах. Heartbeat в одной транзакции продлевает lease задачи и её владельца, а все переходы статусов проверяют token и срок владения.

Квоты на число встреч, backlog и байты проверяются в той же DB-транзакции, что создаёт meeting/task; строка users сериализует параллельные upload одного автора. Повтор того же update_id проверяется до квоты. Файлы, загруженные до введения учёта размера, консервативно учитываются как 20 МБ.

Отдельная recovery goroutine запускается по MEETNOTE_LEASE_RECOVERY_INTERVAL независимо от наличия новых created-задач. Поэтому просроченная задача не голодает при непрерывном backlog. Stale worker не может сохранить результат после передачи lease другой реплике.

Telegram-команды используют отдельную durable inbox. Poller транзакционно сохраняет исходный JSON update и только затем увеличивает offset. Повторная доставка вставляется через ON CONFLICT DO NOTHING. Четыре update-worker забирают записи через FOR UPDATE SKIP LOCKED; отдельная user lease гарантирует максимум одну активную команду на Telegram-пользователя. Поэтому медленная загрузка не мешает другим пользователям.

Мутации дополнительно защищены business idempotency key `telegram:update:<update_id>`: повторный upload возвращает существующую встречу, повторный chat — сохранённый ответ, повторный retry не ставит задачу второй раз.

## Тестовый режим

- для .txt/.md mock Speech использует содержимое файла как транскрипцию;
- для аудио mock Speech возвращает подготовленный текст;
- mock LLM формирует детерминированную выжимку и ответ;
- [speech-error] внутри текстового файла имитирует Speech error;
- [llm-error] имитирует LLM error.

## Конфигурация

| Переменная | По умолчанию |
|---|---|
| MEETNOTE_TELEGRAM_TOKEN | обязательна только для bot |
| MEETNOTE_TELEGRAM_API_URL | https://api.telegram.org |
| MEETNOTE_TELEGRAM_POLL_TIMEOUT | 25s |
| MEETNOTE_TELEGRAM_REQUEST_TIMEOUT | 10s |
| MEETNOTE_TELEGRAM_DOWNLOAD_TIMEOUT | 2m |
| MEETNOTE_TELEGRAM_UPDATE_WORKERS | 4 |
| MEETNOTE_TELEGRAM_UPDATE_LEASE | 3m |
| MEETNOTE_TELEGRAM_UPDATE_MAX_ATTEMPTS | 8 |
| MEETNOTE_TELEGRAM_UPDATE_RETENTION | 168h |
| MEETNOTE_MAX_TRANSCRIPT_RUNES | 500000 |
| MEETNOTE_MAX_SUMMARY_RUNES | 12000 |
| MEETNOTE_MAX_ANSWER_RUNES | 8000 |
| MEETNOTE_MAX_QUESTION_RUNES | 2000 |
| MEETNOTE_MAX_SEARCH_QUERY_RUNES | 500 |
| MEETNOTE_INLINE_TRANSCRIPT_RUNES | 12000 |
| MEETNOTE_DATABASE_URL | локальный PostgreSQL на 55432 |
| MEETNOTE_STORAGE_DIR | ./data/uploads |
| MEETNOTE_MAX_FILE_MB | 20 |
| MEETNOTE_MAX_MEETINGS_PER_USER | 100 |
| MEETNOTE_MAX_PENDING_MEETINGS_PER_USER | 10 |
| MEETNOTE_MAX_STORAGE_MB_PER_USER | 200 |
| MEETNOTE_MAX_CHAT_HISTORY_PER_USER | 1000 |
| MEETNOTE_WORKERS | 3 |
| MEETNOTE_TASK_LEASE | 2m |
| MEETNOTE_LEASE_HEARTBEAT | 20s |
| MEETNOTE_LEASE_RECOVERY_INTERVAL | 10s |
| MEETNOTE_SPEECH_PROVIDER | mock |
| MEETNOTE_LLM_PROVIDER | mock |

Явно заданные некорректные integer/bool/duration значения приводят к fail-fast ошибке, а не молча заменяются default.
Update lease также обязан превышать расчётный budget самой долгой Telegram-команды и записи её результата в PostgreSQL.

## Проверки

~~~bash
make test
make test-race
make vet
make lint
make check
make integration
docker compose config --quiet
~~~

CI запускает go vet, staticcheck v0.7.0 и полный go test -race с PostgreSQL.

## Осознанные ограничения

- запускается одна bot-реплика, workers можно масштабировать независимо;
- локальное хранилище требует общего volume между bot и workers; для нескольких hosts нужен S3-compatible FileStore;
- mock-клиенты демонстрируют архитектуру, а не качество распознавания;
- chat ограничивает контекст десятью последними завершёнными встречами;
- polling PostgreSQL выбран для учебного проекта; при большой нагрузке можно заменить адаптером брокера.
