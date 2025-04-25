// Copyright (c) DeltaStream, Inc.
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	awsconfig "github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/config"
	"github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/util"
)

func updateClusterConfig(ctx context.Context, cfg aws.Config, dp awsconfig.AWSDataplane, infraVersion string, execEngineVersion string) (d diag.Diagnostics) {
	kubeClient, err := util.GetKubeClient(ctx, cfg, dp)
	if err != nil {
		d.AddError("error getting kube client", err.Error())
		return
	}

	ns := &corev1.Namespace{ObjectMeta: v1.ObjectMeta{Name: "cluster-config"}}
	controllerutil.CreateOrUpdate(ctx, kubeClient.Client, ns, func() error {
		return nil
	})

	config, diags := dp.ClusterConfigurationData(ctx)
	d.Append(diags...)
	if d.HasError() {
		return
	}

	cluster, err := util.DescribeKubeCluster(ctx, dp, cfg)
	if err != nil {
		d.AddError("error getting cluster", err.Error())
		return
	}

	promPushProxyUri, err := url.Parse(config.MetricsUrl.ValueString())
	if err != nil {
		d.AddError("error parsing cpPrometheusPushProxyUrl", err.Error())
		return
	}
	kafkaBrokers := []string{}
	diags.Append(config.KafkaHosts.ElementsAs(ctx, &kafkaBrokers, false)...)
	if diags.HasError() {
		return
	}
	// remove port
	for i, b := range kafkaBrokers {
		kafkaBrokers[i] = strings.Split(b, ":")[0]
	}

	kafkaListenerPorts := []string{}
	diags.Append(config.KafkaListenerPorts.ElementsAs(ctx, &kafkaListenerPorts, false)...)
	if diags.HasError() {
		return
	}

	vpcPrivateSubnets := []string{}
	d.Append(config.PrivateLinkSubnetIds.ElementsAs(ctx, &vpcPrivateSubnets, false)...)
	if d.HasError() {
		return
	}
	sort.Strings(vpcPrivateSubnets)

	clusterSubnetIds := []string{}
	d.Append(config.PrivateSubnetIds.ElementsAs(ctx, &clusterSubnetIds, false)...)
	if d.HasError() {
		return
	}
	sort.Strings(clusterSubnetIds)

	clusterPublicSubnetIDs := []string{}
	d.Append(config.PublicSubnetIds.ElementsAs(ctx, &clusterPublicSubnetIDs, false)...)
	if d.HasError() {
		return
	}
	sort.Strings(clusterPublicSubnetIDs)

	customCredentialsEnabled := "disabled"
	if !(config.CustomCredentialsRoleARN.IsNull() || config.CustomCredentialsRoleARN.IsUnknown()) {
		customCredentialsEnabled = "enabled"
	}
	clusterSubnetId0 := clusterSubnetIds[0]
	clusterSubnetId1 := clusterSubnetIds[1]
	clusterSubnetId2 := clusterSubnetId1
	if len(clusterSubnetIds) >= 3 {
		clusterSubnetId2 = clusterSubnetIds[2]
	}
	

	clusterConfig := corev1.Secret{ObjectMeta: v1.ObjectMeta{Name: "cluster-settings", Namespace: "cluster-config"}}
	_, err = controllerutil.CreateOrUpdate(ctx, kubeClient.Client, &clusterConfig, func() error {
		clusterConfig.Data = map[string][]byte{
			"meshID": []byte("deltastream"),
			// todo remove duplicate properties
			"stack":             []byte(config.Stack.ValueString()),
			"environment":       []byte(config.Stack.ValueString()),
			"cloud":             []byte("aws"),
			"region":            []byte(cfg.Region),
			"topology":          []byte("ds"),
			"dsEcrAccountID":    []byte(config.AccountId.ValueString()),
			"execEngineVersion": []byte(execEngineVersion),
			// todo remove duplicate properties
			"awsAccountID":                     []byte(config.AccountId.ValueString()),
			"infraAccountID":                   []byte(fmt.Sprintf("\"%s\"", config.AccountId.ValueString())),
			"infraID":                          []byte(config.InfraId.ValueString()),
			"infraName":                        []byte("ds-" + config.InfraId.ValueString()),
			"infraType":                        []byte(config.InfraType.ValueString()),
			"resourceID":                       []byte(config.EksResourceId.ValueString()),
			"clusterName":                      []byte(*cluster.Name),
			"cpuArchitecture":                  []byte(config.CpuArchitecture.ValueString()),
			"vpnMode":                  		[]byte(config.VpnMode.ValueString()),
			"tailscaleNamespace":               []byte("tailscale-" + config.InfraId.ValueString()),
			"vpcId":                            []byte(config.VpcId.ValueString()),
			"vpcCidr":                          []byte(config.VpcCidr.ValueString()),
			"vpcPrivateSubnetIDs":              []byte(strings.Join(vpcPrivateSubnets, ",")),
			"clusterPrivateSubnetIDs":          []byte(strings.Join(clusterSubnetIds, ",")),
			"clusterPrivateSubnetID1":          []byte(clusterSubnetId0),
			"clusterPrivateSubnetID2":          []byte(clusterSubnetId1),
			"clusterPrivateSubnetID3":          []byte(clusterSubnetId2),
			"clusterPublicSubnetIDs":           []byte(strings.Join(clusterPublicSubnetIDs, ",")),
			"discoveryRegion":                  []byte(cfg.Region),
			"apiServerURI":                     []byte(*cluster.Endpoint),
			"apiServerTokenIssuer":             []byte(*cluster.Identity.Oidc.Issuer),
			"loadbalancerClass":                []byte("service.k8s.aws/nlb"), //hardcode
			"autoscaleMin":                     []byte("3"),                   //hardcode
			"autoscaleMax":                     []byte("5"),                   //hardcode
			"debeziumRoleARN":                  []byte(config.DebeziumRoleArn.ValueString()),
			"externalSecretsRoleARN":           []byte(config.AwsSecretsManagerRoRoleARN.ValueString()),
			"infraOperatorRoleARN":             []byte(config.InfraManagerRoleArn.ValueString()),
			"vaultRoleARN":                     []byte(config.VaultRoleArn.ValueString()),
			"mviewStoreType":					[]byte(config.MaterializedViewStoreType.ValueString()),
			"mviewsRdsCredsSecretName":         []byte(config.RdsMViewsMasterPasswordSecret.ValueString()),
			"vaultInitRoleARN":                 []byte(config.VaultInitRoleArn.ValueString()),
			"lokiRoleARN":                      []byte(config.LokiRoleArn.ValueString()),
			"tempoRoleARN":                     []byte(config.TempoRoleArn.ValueString()),
			"thanosStoreGatewayRoleARN":        []byte(config.ThanosStoreGatewayRoleArn.ValueString()),
			"thanosStoreCompactorRoleARN":      []byte(config.ThanosStoreCompactorRoleArn.ValueString()),
			"thanosStoreBucketWebRoleARN":      []byte(config.ThanosStoreBucketRoleArn.ValueString()),
			"thanosSideCarRoleARN":             []byte(config.ThanosSidecarRoleArn.ValueString()),
			"deadmanAlertRoleARN":              []byte(config.DeadmanAlertRoleArn.ValueString()),
			"karpenterRoleName":                []byte(config.KarpenterNodeRoleName.ValueString()),
			"karpenterIrsaARN":                 []byte(config.KarpenterIrsaRoleArn.ValueString()),
			"storeProxyRoleARN":                []byte(config.StoreProxyRoleArn.ValueString()),
			"interruptionQueueName":            []byte(config.InterruptionQueueName.ValueString()),
			"cw2lokiRoleARN":                   []byte(config.Cw2LokiRoleArn.ValueString()),
			"deltastreamCrossAccountRoleARN":   []byte(config.DsCrossAccountRoleArn.ValueString()),
			"kafkaRoleARN":                     []byte(config.KafkaRoleArn.ValueString()),
			"awsLoadBalancerControllerRoleARN": []byte(config.AwsLoadBalancerControllerRoleARN.ValueString()),
			"additionalApiServerCorsUris":      []byte(config.AdditionalApiServerCorsUris.ValueString()),
			"additionalWebConsoleCorsDomains":  []byte("na"), //hardcode
			"cpPrometheusPushProxyUrl":         []byte(config.MetricsUrl.ValueString()),
			"cpPrometheusPushProxyHost":        []byte(promPushProxyUri.Hostname()),
			"cpPrometheusPushProxyPort":        []byte(`"443"`), //hardcode
			"grafanaVpcHostname":               []byte(config.O11yHostname.ValueString()),
			"ciliumPolicyAuditMode":            []byte("false"),  //hardcode
			"ciliumPolicyEnforcementMode":      []byte("always"), //hardcode

			"grafanaIngressMode": []byte("default"), // deprecated
			"istioIngressMode":   []byte("default"), // deprecated

			"grafanaHostname":            []byte(config.O11yHostname.ValueString()),
			"o11yBucket":                 []byte(config.O11yBucket.ValueString()),
			"o11yEndpointSubnet":         []byte(config.O11ySubnetMode.ValueString()),
			"o11yTlsTermination":         []byte(config.O11yTlsMode.ValueString()),
			"o11yNlbSslCertificate":      []byte(ptr.Deref(config.O11yTlsCertificateArn.ValueStringPointer(), "")),
			"o11yEndpointSecurityGroups": []byte(ptr.Deref(config.O11yIngressSecurityGroups.ValueStringPointer(), "")),

			"apiHostname":                []byte(config.ApiHostname.ValueString()),
			"consoleHostname":            []byte(config.ConsoleHostname.ValueString()),
			"consoleV2Hostname":          []byte(config.ConsoleHostname.ValueString()),
			"downloadsHostname":          []byte(config.DownloadsHostname.ValueString()),
			"platformVersion":            []byte(config.ProductVersion.ValueString()),
			"apiEndpointSubnet":          []byte(config.ApiSubnetMode.ValueString()),
			"apiTlsTermination":          []byte(config.ApiTlsMode.ValueString()),
			"apiServerNlbCertificateArn": []byte(ptr.Deref(config.ApiTlsCertificateArn.ValueStringPointer(), "")),
			"apiEndpointSecurityGroups":  []byte(ptr.Deref(config.ApiIngressSecurityGroups.ValueStringPointer(), "")),

			"grafanaPromPushProxVpcHostname": []byte(config.MetricsUrl.ValueString()),

			"prometheusLocalTSDBRetention": []byte("5d"),    //hardcode
			"prometheusMemoryLimit":        []byte("4Gi"),   //hardcode
			"prometheusPVCStorageSize":     []byte("300Gi"), //hardcode
			"thanosQueryMemoryLimit":       []byte("1.2Gi"), //hardcode
			"thanosStoreMemoryLimit":       []byte("1.2Gi"), //hardcode

			"vpcDnsIP": []byte(config.VpcDnsIP.ValueString()),

			"workloadCredsMode":               []byte(ptr.Deref(config.WorkloadCredentialsMode.ValueStringPointer(), "iamrole")),
			"dpOperatorUserAwsSecret":         []byte(ptr.Deref(config.WorkloadCredentialsSecret.ValueStringPointer(), "")),
			"imageBuildOperatorUserAwsSecret": []byte(ptr.Deref(config.ImageBuildCredentialsSecret.ValueStringPointer(), "")),
			"workloadIamRoleArn":              []byte(ptr.Deref(config.WorkloadRoleArn.ValueStringPointer(), "")),
			"workloadManagerIamRoleArn":       []byte(ptr.Deref(config.WorkloadManagerRoleArn.ValueStringPointer(), "")),

			"customCredentialsRoleARN":      []byte(ptr.Deref(config.CustomCredentialsRoleARN.ValueStringPointer(), "")),
			"enableCustomCredentialsPlugin": []byte(customCredentialsEnabled),
			"rdsCACertsSecret":              []byte(config.RdsCACertsSecret.ValueString()),
			"rdsControlPlaneMasterPasswordSecret":       []byte(config.RdsControlPlaneMasterPasswordSecret.ValueString()),
			"installationTimestamp":         []byte(config.InstallationTimestamp.ValueString()),
		}
		return nil
	})
	if err != nil {
		d.AddError("error setup cluster settings", err.Error())
		return
	}

	return
}
