// Copyright (c) DeltaStream, Inc.
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"net/url"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awsconfig "github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/config"
	"github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/util"
)

func restartNodes(ctx context.Context, dp awsconfig.AWSDataplane, kubeClient *util.RetryableClient) (d diag.Diagnostics) {
	cfg, diags := util.GetAwsConfig(ctx, dp)
	d.Append(diags...)
	if d.HasError() {
		return
	}

	clusterName, err := util.GetKubeClusterName(ctx, dp)
	if err != nil {
		d.AddError("error getting cluster name", err.Error())
		return
	}

	eksClient := eks.NewFromConfig(cfg)
	ec2Client := ec2.NewFromConfig(cfg)

	tflog.Debug(ctx, "listing node groups")
	nodegroupsOutput, err := eksClient.ListNodegroups(ctx, &eks.ListNodegroupsInput{
		ClusterName: &clusterName,
	})
	if err != nil {
		d.AddError("error listing nodegroups", err.Error())
		return
	}
	tflog.Debug(ctx, "found node groups", map[string]any{"nodegroups": nodegroupsOutput.Nodegroups})

	for _, nodegroupName := range nodegroupsOutput.Nodegroups {
		nodes := corev1.NodeList{}
		if err = kubeClient.List(ctx, &nodes, client.MatchingLabels{"eks.amazonaws.com/nodegroup": nodegroupName}); err != nil {
			d.AddError("error listing nodes in nodegroup", err.Error())
			return
		}

		instanceIDs := []string{}
		for _, node := range nodes.Items {
			u, err := url.Parse(node.Spec.ProviderID)
			if err != nil {
				d.AddError("error parsing node provider ID: "+node.Spec.ProviderID, err.Error())
				return
			}
			instanceIDs = append(instanceIDs, filepath.Base(u.Path))
		}
		tflog.Debug(ctx, "found instances in node group", map[string]any{"nodegroup": nodegroupName, "instances": instanceIDs})

		_, err := ec2Client.RebootInstances(ctx, &ec2.RebootInstancesInput{
			InstanceIds: instanceIDs,
		})
		if err != nil {
			d.AddError("error rebooting instances", err.Error())
			return
		}
		tflog.Debug(ctx, "rebooted instances", map[string]any{"nodegroup": nodegroupName, "instances": instanceIDs})
	}
	return
}

func deleteAwsNode(ctx context.Context, cfg aws.Config, dp awsconfig.AWSDataplane) (d diag.Diagnostics) {
	kubeClient, err := util.GetKubeClient(ctx, cfg, dp)
	if err != nil {
		d.AddError("error getting kube client", err.Error())
		return
	}

	nodeRequiresRestart := false
	awsNodeDS := &appsv1.DaemonSet{}
	if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "aws-node"}, awsNodeDS); err != nil {
		if !k8serrors.IsNotFound(err) {
			d.AddError("error getting aws-node DaemonSet", err.Error())
			return
		}
		tflog.Debug(ctx, "aws-node daemonset not found")
		awsNodeDS = nil
	}
	if awsNodeDS != nil {
		nodeRequiresRestart = true
		tflog.Debug(ctx, "deleting aws-node daemonset")
		if err := kubeClient.Delete(ctx, awsNodeDS); err != nil {
			d.AddError("error deleting aws-node DaemonSet", err.Error())
			return
		}
	}
	awsNodeSA := &corev1.ServiceAccount{}
	if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "aws-node"}, awsNodeSA); err != nil {
		if !k8serrors.IsNotFound(err) {
			d.AddError("error getting aws-node DaemonSet", err.Error())
			return
		}
		tflog.Debug(ctx, "aws-node service account not found")
		awsNodeSA = nil
	}
	if awsNodeSA != nil {
		nodeRequiresRestart = true
		tflog.Debug(ctx, "deleting aws-node service account")
		if err := kubeClient.Delete(ctx, awsNodeSA); err != nil {
			d.AddError("error deleting aws-node DaemonSet", err.Error())
			return
		}
	}

	if nodeRequiresRestart {
		if diags := restartNodes(ctx, dp, kubeClient); diags.HasError() {
			d.Append(diags...)
			return
		}
	}

	return
}
