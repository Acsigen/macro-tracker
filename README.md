# Macro Tracker

Macro Tracker is a private health log for one adult. It records nutrition, body measurements, and sleep.

The server uses Go and SQLite. The browser interface uses server HTML, HTMX, and Apache ECharts.

## Start the development server

Copy `.env.example` values into your shell. Set a private password before you start the server.

```sh
export APP_EMAIL=you@example.com
export APP_PASSWORD=replace-this-password
make dev
```

Open `http://localhost:8080`.

## Test and build

```sh
make test
make check
make build
```

The binary is `bin/macro-tracker`.

## Deploy with Docker Compose

Copy `.env.example` to `.env`. Change the email and password in `.env`.

```sh
docker compose config
make deploy
```

The named volume `macro-data` stores the SQLite database. Use `make down` to stop the deployment.

If a TLS proxy serves the site, set `APP_SECURE_COOKIE=true`. The proxy is responsible for TLS.
