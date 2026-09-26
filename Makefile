SHELL := /bin/sh
APP := macro-tracker
VERSION := $(shell tr -d '\n' < VERSION)
IMAGE := acsigen/$(APP):$(VERSION)
GOCACHE := $(CURDIR)/.cache/go-build
GOMODCACHE := $(CURDIR)/.cache/gomod
GO := GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) CGO_ENABLED=0 go

.PHONY: dev test check build image push deploy down

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
	docker build -t $(IMAGE) .

push: image
	docker push $(IMAGE)

deploy:
	MACRO_TRACKER_IMAGE=$(IMAGE) docker compose up -d --build

down:
	docker compose down
