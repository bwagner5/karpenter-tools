BUILD_DIR ?= $(dir $(realpath -s $(firstword $(MAKEFILE_LIST))))/build
VERSION ?= $(shell git describe --tags --always --dirty)
GOOS ?= $(shell uname | tr '[:upper:]' '[:lower:]')
GOARCH ?= $(shell [[ `uname -m` = "x86_64" ]] && echo "amd64" || echo "arm64" )
GOPROXY ?= "https://proxy.golang.org,direct"

$(shell mkdir -p ${BUILD_DIR})

all: verify test build

build: ## build binary using current OS and Arch
	go build -a -ldflags="-s -w -X main.versionID=${VERSION}" -o ${BUILD_DIR}/chk-sched-${GOOS}-${GOARCH} ${BUILD_DIR}/../cmd/main.go

test: ## run go tests and benchmarks
	go test -bench=. ${BUILD_DIR}/../... -v -coverprofile=coverage.out -covermode=atomic -outputdir=${BUILD_DIR}

verify: ## run depedency verification, download and source formatting and vetting
	go mod tidy
	go mod download
	go vet ./...
	go fmt ./...

version: ## Output version of local HEAD
	@echo ${VERSION}

help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

.PHONY: all build test verify help