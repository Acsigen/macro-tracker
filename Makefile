SHELL := /bin/sh
APP := macro-tracker
GOCACHE := $(CURDIR)/.cache/go-build
GOMODCACHE := $(CURDIR)/.cache/gomod
GO := GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) CGO_ENABLED=0 go

.PHONY: dev test check build image deploy down

dev:
	$(GO) run .

test:
	$(GO) test ./...

check:
	$(GO) vet ./...
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) gopls check main.go main_test.go

build:
	mkdir -p bin
	$(GO) build -trimpath -ldflags="-s -w" -o bin/$(APP) .

image:
	docker build -t $(APP):$$(tr -d '\n' < VERSION) .

deploy:
	docker compose up -d --build

down:
	docker compose down
