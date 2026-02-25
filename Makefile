default: download_assets fmt testacc
	CGO_ENABLED=0 go install -tags containers_image_openpgp .

.PHONY: fmt
fmt:
	terraform fmt -recursive ./examples/

.PHONY: doc
doc:
	go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs generate -provider-name platform

.PHONY: testacc
testacc:
	CGO_ENABLED=0 TF_ACC=1 go test -tags containers_image_openpgp -v ./... -v $(TESTARGS) -timeout 120m

clean:
	rm -rf docs
	rm internal/deltastream/aws/assets/cilium-*.tgz
	rm internal/deltastream/aws/assets/flux-system/gotk-components.yaml
	rm internal/deltastream/aws/assets/flux-system/flux.yaml.tmpl

download_assets:
	- helm repo add cilium https://helm.cilium.io/
	helm repo update
	cd internal/deltastream/aws/assets && helm pull cilium/cilium --version 1.16.1
	cd internal/deltastream/aws/assets/flux-system && \
		flux install --version=v2.7.5 --network-policy=false --export > gotk-components.yaml && \
		kustomize build . > flux.yaml.tmpl
