default: fmt doc testacc

.PHONY: fmt
fmt:
	terraform fmt -recursive ./examples/

.PHONY: doc
doc:
	go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs generate -provider-name deltastream-dataplane

.PHONY: testacc
testacc:
	TF_ACC=1 go test -v ./... -v $(TESTARGS) -timeout 120m

