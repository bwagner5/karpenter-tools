help: ## Display help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

verify: codegen ## Verify code. Includes dependencies, linting, formatting, etc
	go mod tidy
	go mod download
	golangci-lint run

apply: ## Deploy the controller into your ~/.kube/config cluster
	helm upgrade --install ctd charts/capacity-type-distribution --namespace ctd \
		--set webhook.image=$(shell ko build -B github.com/bwagner5/karpenter-tools/capacity-distribution/cmd/ctd) 

delete: ## Delete the controller from your ~/.kube/config cluster
	helm uninstall ctd --namespace ctd

.PHONY: help dev ci release test battletest verify codegen apply delete toolchain release licenses issues website
