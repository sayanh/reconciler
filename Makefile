APP_NAME = reconciler
IMG_NAME := $(DOCKER_PUSH_REPOSITORY)$(DOCKER_PUSH_DIRECTORY)/$(APP_NAME)
TAG := $(DOCKER_TAG)
COMPONENTS := $(shell recons=$$(find pkg/reconciler/instances -d 1 -type d -not -path '*/example' -execdir echo -n '{} ' \; | xargs) && echo $${recons// /,})

ifndef VERSION
	VERSION = ${shell git describe --tags --always}
endif

ifeq ($(VERSION),stable)
	VERSION = stable-${shell git rev-parse --short HEAD}
endif

.DEFAULT_GOAL=all

.PHONY: resolve
resolve:
	go mod tidy

.PHONY: lint
lint:
	./scripts/lint.sh

.PHONY: build
build: build-linux build-darwin build-linux-arm

.PHONY: build-linux
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ./bin/reconciler-linux $(FLAGS) ./cmd

.PHONY: build-linux-arm
build-linux-arm:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o ./bin/reconciler-arm $(FLAGS) ./cmd

.PHONY: build-darwin
build-darwin:
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -o ./bin/reconciler-darwin $(FLAGS) ./cmd

.PHONY: docker-build
docker-build:
	docker build -t $(APP_NAME):latest .

.PHONY: docker-push
docker-push:
	docker tag $(APP_NAME) $(IMG_NAME):$(TAG)
	docker push $(IMG_NAME):$(TAG)

.PHONY: deploy
deploy:
	kubectl create namespace reconciler --dry-run=client -o yaml | kubectl apply -f -
	@echo "components: ${COMPONENTS}"
	helm template reconciler --namespace reconciler --set "global.components={base,istio}" ./resources/reconciler > reconciler.yaml
	kubectl apply -f reconciler.yaml
	rm reconciler.yaml

.PHONY: test
test:
	go test -race -coverprofile=cover.out ./...
	@echo "Total test coverage: $$(go tool cover -func=cover.out | grep total | awk '{print $$3}')"
	@rm cover.out

e2e-test:
	kubectl run -n reconciler --image=alpine:3.14.1 --restart=Never test-pod -- sh -c "sleep 36000"

	kubectl run -n reconciler --image=alpine:3.14.1 --restart=Never test-pod -- sh -c "set -eu;apk add curl; curl http://reconciler-mothership-reconciler.reconciler"


.PHONY: test-all
test-all: export RECONCILER_EXPENSIVE_TESTS = 1
test-all: test

.PHONY: clean
clean:
	rm -rf bin

.PHONY: all
all: resolve build test lint docker-build docker-push

.PHONY: release
release: all
