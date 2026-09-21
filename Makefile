IMAGE     ?= ghcr.io/kenan435/cx-metrics-gen
TAG       ?= dev
NAMESPACE ?= cx-metrics-gen
# Target cluster. Leave empty to use the current context, or set it explicitly
# so a deploy cannot land on whatever context happened to be selected:
#   make deploy CONTEXT=kenan-lab
CONTEXT   ?=
# Which kustomize directory to apply. Override for a cluster-specific overlay:
#   make deploy OVERLAY=deploy/k8s/overlays/kenan-lab CONTEXT=kenan-lab
OVERLAY   ?= deploy/k8s/base
KUBECTL   := kubectl $(if $(CONTEXT),--context $(CONTEXT),)
PLATFORMS ?= linux/amd64,linux/arm64

.PHONY: test vet fmt build run image image-push deploy secret logs status undeploy clean

test:
	go test ./... -count=1

vet:
	go vet ./...

fmt:
	gofmt -w .

# Local binary, for running outside a container.
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o cx-metrics-gen .

# Run against Coralogix from this machine. Needs CORALOGIX_API_KEY in the env.
run:
	go run .

image:
	docker build -t $(IMAGE):$(TAG) .

# CI does this on every push to main. Only needed for a manual out of band build.
image-push:
	docker buildx build --platform $(PLATFORMS) -t $(IMAGE):$(TAG) --push .

# Create the API key secret. Usage: make secret CORALOGIX_API_KEY=cxtp_xxx
secret:
	@test -n "$(CORALOGIX_API_KEY)" || (echo "set CORALOGIX_API_KEY" && exit 1)
	$(KUBECTL) create namespace $(NAMESPACE) --dry-run=client -o yaml | $(KUBECTL) apply -f -
	$(KUBECTL) -n $(NAMESPACE) create secret generic cx-metrics-gen \
		--from-literal=CORALOGIX_API_KEY=$(CORALOGIX_API_KEY) \
		--dry-run=client -o yaml | $(KUBECTL) apply -f -

deploy:
	$(KUBECTL) apply -k $(OVERLAY)

logs:
	$(KUBECTL) -n $(NAMESPACE) logs -l app.kubernetes.io/name=cx-metrics-gen -f --tail=50

status:
	$(KUBECTL) -n $(NAMESPACE) port-forward svc/cx-metrics-gen 8080:8080

undeploy:
	$(KUBECTL) delete -k $(OVERLAY) --ignore-not-found

clean:
	rm -f cx-metrics-gen
