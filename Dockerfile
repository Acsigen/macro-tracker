FROM golang:1.27.1-alpine3.24 AS build
RUN apk add --no-cache make
WORKDIR /src
COPY go.mod go.sum ./
COPY . .
RUN make build

FROM alpine:3.24.2
RUN apk add --no-cache ca-certificates && addgroup -S app && adduser -S -G app app && mkdir /data && chown app:app /data
COPY --from=build /src/bin/macro-tracker /usr/local/bin/macro-tracker
USER app
VOLUME /data
EXPOSE 8080
ENV APP_ADDR=:8080 APP_DB_PATH=/data/macro-tracker.db APP_TIMEZONE=Europe/Madrid APP_SECURE_COOKIE=true
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["macro-tracker"]
