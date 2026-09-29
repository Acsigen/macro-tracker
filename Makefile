SHELL := /bin/sh
APP := macro-tracker
VERSION := $(shell tr -d '\n' < VERSION)
IMAGE := acsigen/$(APP):$(VERSION)
GOCACHE := $(CURDIR)/.cache/go-build
GOMODCACHE := $(CURDIR)/.cache/gomod
GO := GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) CGO_ENABLED=0 go
TEST_FLAGS ?=

.PHONY: dev test test-race test-js check build image push deploy down

dev:
	$(GO) run .

test:
	$(GO) test $(value TEST_FLAGS) ./...

# Go's race detector requires CGO; the application and SQLite driver do not.
test-race:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) CGO_ENABLED=1 go test -race $(value TEST_FLAGS) ./...

test-js:
	node --test tests/*.test.cjs

check:
	$(GO) vet ./...
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) gopls check main.go *_test.go

build:
	mkdir -p bin
	$(GO) build -trimpath -ldflags="-s -w" -o bin/$(APP) .

image:
	docker build --platform linux/amd64 -t $(IMAGE) .

push: image
	docker push $(IMAGE)

deploy:
	MACRO_TRACKER_IMAGE=$(IMAGE) docker compose up -d --build

down:
	docker compose down
