package aws

import (
	"bytes"
	"context"
	"html/template"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	awsconfig "github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/config"
	"github.com/hashicorp/terraform-plugin-framework/diag"
)

var trustRelationTemplate = `
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Principal": {
                "Federated": "arn:aws:iam::{{ .Account }}:oidc-provider/oidc.eks.{{ .Region }}.amazonaws.com/id/{{ .OIDCIdentifier }}"
            },
            "Action": "sts:AssumeRoleWithWebIdentity",
            "Condition": {
                "StringEquals": {
                    "oidc.eks.{{ .Region }}.amazonaws.com/id/{{ .OIDCIdentifier }}:aud": "sts.amazonaws.com",
                    "oidc.eks.{{ .Region }}.amazonaws.com/id/{{ .OIDCIdentifier }}:sub": "system:serviceaccount:{{ .SvcNamespace }}:{{ .SvcName }}"
                }
            }
        }
    ]
}`

func updateRoleTrustPolicy(ctx context.Context, cfg aws.Config, clusterConfig awsconfig.ClusterConfiguration, issuerID, roleArn, serviceAccountName, serviceAccountNamespace string) (d diag.Diagnostics) {
	var b bytes.Buffer
	arnParts := strings.Split(roleArn, "/")
	roleName := arnParts[len(arnParts)-1]

	trustRelationTmpl := template.Must(template.New("trustRelation").Parse(trustRelationTemplate))
	if err := trustRelationTmpl.Execute(&b, map[string]any{
		"Account":        clusterConfig.AccountId.ValueString(),
		"Region":         cfg.Region,
		"OIDCIdentifier": issuerID,
		"SvcNamespace":   serviceAccountNamespace,
		"SvcName":        serviceAccountName,
	}); err != nil {
		d.AddError("failed to render trust relation template for role "+roleName, err.Error())
		return
	}

	iamclient := iam.NewFromConfig(cfg)
	if _, err := iamclient.UpdateAssumeRolePolicy(ctx, &iam.UpdateAssumeRolePolicyInput{
		RoleName:       aws.String(roleName),
		PolicyDocument: aws.String(strings.TrimSpace(b.String())),
	}); err != nil {
		d.AddError("failed to update role trust relation for role "+roleName, err.Error())
		return
	}

	return
}
