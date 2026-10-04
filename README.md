# Macro Tracker

Macro Tracker is a private health log for one adult. It records nutrition, body measurements, and sleep.

The server uses Go and SQLite. The browser interface uses server HTML, HTMX, and Apache ECharts.

## Dependencies

Use Go 1.27.1 or later and Make for development. JavaScript tests also require Node.js.

The Docker build uses Go 1.27.1 on Alpine 3.24. The runtime image uses Alpine 3.24.2.

Go module versions are pinned in `go.mod` and authenticated through `go.sum`. SQLite uses `modernc.org/sqlite` v1.60.1 without CGO.

The browser bundles HTMX 4.0.0 and Apache ECharts 6.1.0 locally. Their license files reside in `assets/licenses`.

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
make test-js
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

## Configure AI food analysis

Sign in. Open Settings. In the AI section, enter your gateway base URL, model ID, and API key.

The base URL defaults to `https://deepseek.dait.es/v1`. The server sends requests to `https://deepseek.dait.es/v1/chat/completions`.

Use a Pangolin resource that supports the OpenAI Chat Completions format. Enter the exact model ID that your resource permits.

The app requests strict JSON Schema output first. If the gateway explicitly rejects this format, the app retries once with JSON instructions alone.

This retry supports DeepSeek models that reject `json_schema`, including `deepseek-flash`. The server applies the same nutrition rules to both responses.

Save the AI configuration. Then select Test connection. This action sends a sample nutrition request and can incur model usage charges.

If the gateway rejects a request, the app shows its JSON error message with credential details removed. Use this message to investigate the failure.

A blank API key field preserves the saved key. Select Clear the saved API key to remove it.

A private gateway can work without a key. The server must have network access to the gateway.

## Log and review food

Open Nutrition. Select New food or Library food.

For a new food, enter the date, food description, and edible portion weight in grams. Include the preparation, brand, and ingredients when known.

Select Analyze food. Read the estimated portion values, values per 100 g, and preparation assumptions. Then select Save food entry.

For a library food, select the saved description and enter the date and portion weight. Select Save food entry.

The app saves previously analyzed library foods directly, without another review or model request. Older manual foods still need analysis and review first.

The app accepts descriptions in English or Spanish, up to 2,000 characters. It uses model knowledge with Spain and EU instructions.

The estimates are not verified food label values. The model must provide all tracked nutrients, including omega 3 and omega 6 amounts.

If the model cannot estimate an omega amount, revise the description and try again. The app saves nothing when analysis fails.

The app reuses saved AI analysis for the same description, even when capitalization differs or the description has surrounding spaces.

This matching supports accented Romanian and Spanish letters. The review states when it uses saved analysis, without contacting the AI gateway.

Select Reanalyze in the library to request new estimates.

Old manual foods require AI review before reuse. Past entries retain their saved nutrition after library changes or deletion.

Date and weight edits use the entry’s saved nutrition. Description changes require another review.

Reviews expire after 15 minutes or a server restart. If a review expires, preview or analyze the food again.

The daily omega ratio uses the summed omega amounts for all portions. It remains incomplete when an old entry lacks omega data.

Measurements, nutrients, and ratios use two decimal places on screen. The database keeps full precision for calculations.

The AI estimates omega 3 and omega 6 amounts. The app calculates their ratio and ignores any ratio returned by the AI.

The ratio is a single number: omega 3 divided by omega 6. For example, 1 g divided by 2 g gives `0.50`.

If omega 6 is zero and omega 3 is positive, the app shows “Undefined (omega 6 is zero)”.

The app shows “No omega fats” when both amounts are zero.

The Charts page shows the daily omega 3 to omega 6 ratio over time. Each point uses that day’s summed portion amounts.

Days with incomplete omega data, no omega fats, or zero omega 6 appear as gaps in the chart.

## Back up AI credentials and nutrition

The server encrypts the API key in SQLite. It stores the encryption key in `ai-secret.key` beside the database, with file permissions `0600`.

For Docker Compose, both files reside in the `macro-data` volume under `/data`. Back up the database and `ai-secret.key` together.

For a consistent SQLite backup, use the SQLite backup API. If you copy database files, stop the app first. Keep backups private.

If `ai-secret.key` is missing, restore it from your backup. You can also enter a replacement API key in Settings.

The app does not regenerate the encryption file merely because it cannot read a saved key. A replacement key explicitly creates a new file when needed.

The API key remains on the server and never appears in page HTML or gateway error messages.
