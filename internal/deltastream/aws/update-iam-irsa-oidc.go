// Copyright (c) DeltaStream, Inc.
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"k8s.io/utils/ptr"

	awsconfig "github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/config"
	"github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/util"
)

func updateIamRolesOIDC(ctx context.Context, cfg aws.Config, dp awsconfig.AWSDataplane) (d diag.Diagnostics) {

	config, _ := dp.ClusterConfigurationData(ctx)
	cluster, err := util.DescribeKubeCluster(ctx, dp, cfg)
	if err != nil {
		d.AddError("failed to describe EKS cluster", err.Error())
		return
	}

	issArr := strings.Split(ptr.Deref(cluster.Identity.Oidc.Issuer, ""), "/")
	issuerID := issArr[len(issArr)-1]

	roles, err := listRolesByTag(ctx, cfg, "deltastream-io-name", fmt.Sprintf("ds-%s", config.InfraId.ValueString()))
	if err != nil {
		d.AddError("unable to list roles", err.Error())
		return
	}

	for _, roleName := range roles {
		err := util.UpdateEmptyOIDCRoleTrustPolicy(ctx, cfg, issuerID, roleName)
		if err != nil {
			d.AddError(fmt.Sprintf("unable to update Empty OIDC trust for iam role %s", roleName), err.Error())
			return
		}
	}
	return

}

func listRolesByTag(ctx context.Context, cfg aws.Config, tagName string, tagValue string) ([]string, error) {
	client := iam.NewFromConfig(cfg)

	var roleNames []string
	var marker *string

	for {
		output, err := client.ListRoles(ctx, &iam.ListRolesInput{
			Marker: marker,
		})
		if err != nil {
			return nil, err
		}

		for _, role := range output.Roles {
			tagsOutput, err := client.ListRoleTags(ctx, &iam.ListRoleTagsInput{
				RoleName: role.RoleName,
			})
			if err != nil {
				return nil, err
			}

			for _, tag := range tagsOutput.Tags {
				if *tag.Key == tagName && *tag.Value == tagValue {
					roleNames = append(roleNames, *role.RoleName)
					break
				}
			}
		}

		if output.Marker == nil {
			break
		}
		marker = output.Marker
	}
	return roleNames, nil
}
