IMAGE_TAG ?= latest
REGISTRY ?= ghcr.io/lightwell-tech
IMAGE_NAME ?= ahv-to-ove-operator
NAMESPACE ?= ahv-to-ove-operator-system
IMG ?= $(REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)

VERSION ?= 0.1.0
BUNDLE_IMG ?= $(REGISTRY)/ahv-to-ove-operator-bundle:v$(VERSION)

# Optional build-time proxy for corporate-proxy networks. Example:
#   make docker-push PROXY_ARGS="--build-arg HTTPS_PROXY=http://proxy.example.com:8080 \
#     --build-arg HTTP_PROXY=http://proxy.example.com:8080 --build-arg NO_PROXY=localhost,127.0.0.1"
PROXY_ARGS ?=

.PHONY: all
all: build

## Build
.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: build
build: fmt vet
	go build -o bin/manager main.go

.PHONY: run
run: fmt vet
	./bin/manager --leader-elect=false

## Docker / OCP image
.PHONY: docker-login
docker-login:
	@echo "Log in to $(REGISTRY) before pushing. For GHCR:"
	@echo '  echo $$GHCR_TOKEN | docker login ghcr.io -u <github-user> --password-stdin'

.PHONY: docker-build
docker-build:
	docker buildx build --provenance=false $(PROXY_ARGS) -t $(IMG) .

.PHONY: docker-push
docker-push:
	docker buildx build --provenance=false $(PROXY_ARGS) --push -t $(IMG) .

# ワンコマンドでビルド + プッシュ + Pod ローリング更新
.PHONY: deploy-image
deploy-image: docker-push
	oc rollout restart deployment/ahv-to-ove-operator-controller-manager -n $(NAMESPACE)
	oc rollout status deployment/ahv-to-ove-operator-controller-manager -n $(NAMESPACE)

## Deploy CRD + RBAC + Deployment
.PHONY: install-crd
install-crd:
	oc apply -f config/crd/bases/

.PHONY: uninstall-crd
uninstall-crd:
	oc delete -f config/crd/bases/

.PHONY: deploy
deploy:
	oc create namespace $(NAMESPACE) --dry-run=client -o yaml | oc apply -f -
	oc apply -f config/crd/bases/
	oc apply -f config/rbac/role.yaml
	oc apply -f config/manager/deployment.yaml
	oc rollout status deployment/ahv-to-ove-operator-controller-manager -n $(NAMESPACE)

.PHONY: undeploy
undeploy:
	oc delete -f config/manager/deployment.yaml --ignore-not-found
	oc delete -f config/rbac/role.yaml --ignore-not-found
	oc delete -f config/crd/bases/ --ignore-not-found
	oc delete namespace $(NAMESPACE) --ignore-not-found

## Migration management
.PHONY: status
status:
	oc get ahvmigration -n $(NAMESPACE) -o wide
	@echo ""
	oc get datavolume -n $(NAMESPACE) 2>/dev/null || true
	@echo ""
	oc get vm -n $(NAMESPACE) 2>/dev/null || true

.PHONY: watch
watch:
	watch -n 5 "oc get ahvmigration -n $(NAMESPACE) -o wide"

.PHONY: logs
logs:
	oc logs -n $(NAMESPACE) -l control-plane=controller-manager -f

## OLM Bundle
.PHONY: bundle
bundle:
	cp config/crd/bases/*.yaml bundle/manifests/
	@echo "Bundle ready at bundle/. Verify with: operator-sdk bundle validate ./bundle"

.PHONY: bundle-build
bundle-build:
	docker build -f bundle/Dockerfile -t $(BUNDLE_IMG) bundle/

.PHONY: bundle-push
bundle-push:
	docker push $(BUNDLE_IMG)

.PHONY: bundle-validate
bundle-validate:
	operator-sdk bundle validate ./bundle

## Release
.PHONY: release
release: bundle bundle-build bundle-push
	@echo "Bundle $(BUNDLE_IMG) built and pushed."
