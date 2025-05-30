// Copyright (c) DeltaStream, Inc.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"context"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
)

type AWSDataplane struct {
	AssumeRole           basetypes.ObjectValue `tfsdk:"assume_role"`
	ClusterConfiguration basetypes.ObjectValue `tfsdk:"configuration"`
	Status               basetypes.ObjectValue `tfsdk:"status"`
}

type AssumeRole struct {
	RoleArn     basetypes.StringValue `tfsdk:"role_arn"`
	SessionName basetypes.StringValue `tfsdk:"session_name"`
	Region      basetypes.StringValue `tfsdk:"region"`
}

type Status struct {
	ProviderVersion basetypes.StringValue `tfsdk:"provider_version"`
	ProductVersion  basetypes.StringValue `tfsdk:"product_version"`
	LastModified    basetypes.StringValue `tfsdk:"last_modified"`
}

func (m Status) AttributeTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"provider_version": types.StringType,
		"product_version":  types.StringType,
		"last_modified":    types.StringType,
	}
}

type ClusterConfiguration struct {
	Stack       basetypes.StringValue `tfsdk:"stack"`
	DsAccountId basetypes.StringValue `tfsdk:"ds_account_id"`
	DsRegion    basetypes.StringValue `tfsdk:"ds_region"`

	AccountId             basetypes.StringValue `tfsdk:"account_id"`
	InfraId               basetypes.StringValue `tfsdk:"infra_id"`
	InfraType             basetypes.StringValue `tfsdk:"infra_type"`
	EksResourceId         basetypes.StringValue `tfsdk:"eks_resource_id"`
	ClusterIndex          basetypes.Int64Value  `tfsdk:"cluster_index"`
	ProductVersion        basetypes.StringValue `tfsdk:"product_version"`
	CpuArchitecture       basetypes.StringValue `tfsdk:"cpu_architecture"`
	NodepoolInstanceTypes basetypes.ListValue   `tfsdk:"nodepool_instance_types"`
	NodepoolCapacityType  basetypes.StringValue `tfsdk:"nodepool_capacity_type"`
	NodepoolCpuLimit      basetypes.Int32Value  `tfsdk:"nodepool_cpu_limit"`

	VpcId    basetypes.StringValue `tfsdk:"vpc_id"`
	VpcCidr  basetypes.StringValue `tfsdk:"vpc_cidr"`
	VpcDnsIP basetypes.StringValue `tfsdk:"vpc_dns_ip"`
	VpnMode  basetypes.StringValue `tfsdk:"vpn_mode"`

	PrivateLinkSubnetIds basetypes.ListValue `tfsdk:"private_link_subnets_ids"`

	PrivateSubnetIds       basetypes.ListValue   `tfsdk:"private_subnet_ids"`
	PublicSubnetIds        basetypes.ListValue   `tfsdk:"public_subnet_ids"`
	MetricsUrl             basetypes.StringValue `tfsdk:"metrics_url"`
	InterruptionQueueName  basetypes.StringValue `tfsdk:"interruption_queue_name"`
	ProductArtifactsBucket basetypes.StringValue `tfsdk:"product_artifacts_bucket"`
	SerdeBucket            basetypes.StringValue `tfsdk:"serde_bucket"`
	FunctionsBucket        basetypes.StringValue `tfsdk:"functions_bucket"`
	WorkloadStateBucket    basetypes.StringValue `tfsdk:"workload_state_bucket"`
	O11yBucket             basetypes.StringValue `tfsdk:"o11y_bucket"`
	OrbBillingBucket       basetypes.StringValue `tfsdk:"orb_billing_bucket"`
	OrbBillingBucketRegion basetypes.StringValue `tfsdk:"orb_billing_bucket_region"`

	AwsSecretsManagerRoRoleARN       basetypes.StringValue `tfsdk:"aws_secrets_manager_ro_role_arn"`
	DebeziumRoleArn                  basetypes.StringValue `tfsdk:"debezium_role_arn"`
	InfraManagerRoleArn              basetypes.StringValue `tfsdk:"infra_manager_role_arn"`
	VaultRoleArn                     basetypes.StringValue `tfsdk:"vault_role_arn"`
	VaultInitRoleArn                 basetypes.StringValue `tfsdk:"vault_init_role_arn"`
	LokiRoleArn                      basetypes.StringValue `tfsdk:"loki_role_arn"`
	TempoRoleArn                     basetypes.StringValue `tfsdk:"tempo_role_arn"`
	ThanosStoreGatewayRoleArn        basetypes.StringValue `tfsdk:"thanos_store_gateway_role_arn"`
	ThanosStoreCompactorRoleArn      basetypes.StringValue `tfsdk:"thanos_store_compactor_role_arn"`
	ThanosStoreBucketRoleArn         basetypes.StringValue `tfsdk:"thanos_store_bucket_role_arn"`
	ThanosSidecarRoleArn             basetypes.StringValue `tfsdk:"thanos_sidecar_role_arn"`
	DeadmanAlertRoleArn              basetypes.StringValue `tfsdk:"deadman_alert_role_arn"`
	KarpenterNodeRoleName            basetypes.StringValue `tfsdk:"karpenter_node_role_name"`
	KarpenterIrsaRoleArn             basetypes.StringValue `tfsdk:"karpenter_irsa_role_arn"`
	StoreProxyRoleArn                basetypes.StringValue `tfsdk:"store_proxy_role_arn"`
	Cw2LokiRoleArn                   basetypes.StringValue `tfsdk:"cw2loki_role_arn"`
	EcrReadonlyRoleArn               basetypes.StringValue `tfsdk:"ecr_readonly_role_arn"`
	DsCrossAccountRoleArn            basetypes.StringValue `tfsdk:"ds_cross_account_role_arn"`
	KafkaRoleArn                     basetypes.StringValue `tfsdk:"kafka_role_arn"`
	KafkaRoleExternalId              basetypes.StringValue `tfsdk:"kafka_role_external_id"`
	AwsLoadBalancerControllerRoleARN basetypes.StringValue `tfsdk:"aws_load_balancer_controller_role_arn"`
	AdditionalApiServerCorsUris      basetypes.StringValue `tfsdk:"additional_api_server_cors_uris"`
	CustomCredentialsRoleARN         basetypes.StringValue `tfsdk:"custom_credentials_role_arn"`
	CustomCredentialsImage           basetypes.StringValue `tfsdk:"custom_credentials_image"`

	WorkloadCredentialsMode     basetypes.StringValue `tfsdk:"workload_credentials_mode"`
	WorkloadCredentialsSecret   basetypes.StringValue `tfsdk:"workload_credentials_secret"`
	ImageBuildCredentialsSecret basetypes.StringValue `tfsdk:"image_build_credentials_secret"`
	WorkloadRoleArn             basetypes.StringValue `tfsdk:"workload_role_arn"`
	WorkloadManagerRoleArn      basetypes.StringValue `tfsdk:"workload_manager_role_arn"`

	O11yHostname              basetypes.StringValue `tfsdk:"o11y_hostname"`
	O11ySubnetMode            basetypes.StringValue `tfsdk:"o11y_subnet_mode"`
	O11yTlsMode               basetypes.StringValue `tfsdk:"o11y_tls_mode"`
	O11yTlsCertificateArn     basetypes.StringValue `tfsdk:"o11y_tls_certificate_arn"`
	O11yIngressSecurityGroups basetypes.StringValue `tfsdk:"o11y_ingress_security_groups"`

	ApiHostname              basetypes.StringValue `tfsdk:"api_hostname"`
	ApiSubnetMode            basetypes.StringValue `tfsdk:"api_subnet_mode"`
	ApiTlsMode               basetypes.StringValue `tfsdk:"api_tls_mode"`
	ApiTlsCertificateArn     basetypes.StringValue `tfsdk:"api_tls_certificate_arn"`
	ApiIngressSecurityGroups basetypes.StringValue `tfsdk:"api_ingress_security_groups"`

	KmsKeyId          basetypes.StringValue `tfsdk:"kms_key_id"`
	DynamoDbTableName basetypes.StringValue `tfsdk:"dynamodb_table_name"`

	KafkaHosts         basetypes.ListValue   `tfsdk:"kafka_hosts"`
	KafkaListenerPorts basetypes.ListValue   `tfsdk:"kafka_listener_ports"`
	KafkaClusterName   basetypes.StringValue `tfsdk:"kafka_cluster_name"`

	RdsControlPlaneResourceID basetypes.StringValue `tfsdk:"rds_control_plane_resource_id"`
	RdsMViewsResourceID       basetypes.StringValue `tfsdk:"rds_mviews_resource_id"`
	RdsMViewsUsingAurora      basetypes.BoolValue   `tfsdk:"rds_mviews_using_aurora"`
	MaterializedViewStoreType basetypes.StringValue `tfsdk:"materialized_view_store_type"`

	Cw2LokiSqsUrl basetypes.StringValue `tfsdk:"cw2loki_sqs_url"`

	ConsoleHostname                     basetypes.StringValue `tfsdk:"console_hostname"`
	DownloadsHostname                   basetypes.StringValue `tfsdk:"downloads_hostname"`
	RdsCACertsSecret                    basetypes.StringValue `tfsdk:"rds_ca_certs_secret"`
	RdsControlPlaneMasterPasswordSecret basetypes.StringValue `tfsdk:"rds_control_plane_master_password_secret"`
	RdsControlPlaneDatabaseName         basetypes.StringValue `tfsdk:"rds_control_plane_database_name"`
	RdsControlPlaneHostName             basetypes.StringValue `tfsdk:"rds_control_plane_host_name"`
	RdsControlPlaneHostPort             basetypes.Int64Value  `tfsdk:"rds_control_plane_host_port"`

	RdsMViewsMasterPasswordSecret basetypes.StringValue `tfsdk:"rds_mviews_master_password_secret"`
	RdsMViewsDatabaseName         basetypes.StringValue `tfsdk:"rds_mviews_database_name"`
	RdsMViewsHostName             basetypes.StringValue `tfsdk:"rds_mviews_host_name"`
	RdsMViewsHostPort             basetypes.Int64Value  `tfsdk:"rds_mviews_host_port"`

	Auth0LoginConfig basetypes.StringValue `tfsdk:"auth0_login_config"`

	InstallationTimestamp basetypes.StringValue `tfsdk:"installation_timestamp"`
}

func (d *AWSDataplane) AssumeRoleData(ctx context.Context) (AssumeRole, diag.Diagnostics) {
	var ar AssumeRole
	diag := d.AssumeRole.As(ctx, &ar, basetypes.ObjectAsOptions{})
	return ar, diag
}

func (d *AWSDataplane) ClusterConfigurationData(ctx context.Context) (ClusterConfiguration, diag.Diagnostics) {
	var cc ClusterConfiguration
	diag := d.ClusterConfiguration.As(ctx, &cc, basetypes.ObjectAsOptions{})

	if cc.Stack.IsNull() || cc.Stack.IsUnknown() {
		cc.Stack = basetypes.NewStringValue("prod")
	}

	return cc, diag
}

var Schema = schema.Schema{
	MarkdownDescription: "AWS Dataplane resource",

	Attributes: map[string]schema.Attribute{
		"assume_role": schema.SingleNestedAttribute{
			Description: "Assume role configuration",
			Required:    true,
			Attributes: map[string]schema.Attribute{
				"role_arn": schema.StringAttribute{
					Description: "Amazon Resource Name (ARN) of an IAM Role to assume prior to making API calls.",
					Optional:    true,
				},
				"session_name": schema.StringAttribute{
					Description: "An identifier for the assumed role session.",
					Optional:    true,
				},
				"region": schema.StringAttribute{
					Description: "The AWS region to use for the assume role.",
					Optional:    true,
				},
			},
		},
		"configuration": schema.SingleNestedAttribute{
			Description: "Cluster configuration",
			Required:    true,
			Attributes: map[string]schema.Attribute{
				"stack": schema.StringAttribute{
					Description: "The type of DeltaStream dataplane (default: prod).",
					Optional:    true,
				},
				"ds_account_id": schema.StringAttribute{
					Description: "The account ID provided by DeltaStream.",
					Required:    true,
				},
				"ds_region": schema.StringAttribute{
					Description: "The AWS region provided by DeltaStream.",
					Optional:    true,
				},

				"account_id": schema.StringAttribute{
					Description: "The account ID hosting the DeltaStream dataplane.",
					Required:    true,
				},
				"infra_id": schema.StringAttribute{
					Description: "The infra ID of the DeltaStream dataplane (provided by DeltaStream).",
					Required:    true,
				},
				"infra_type": schema.StringAttribute{
					Description: "The infra Type for DeltaStream dataplane.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.OneOf("trial_multi_tenant", "byoc", "dedicated")},
				},
				"eks_resource_id": schema.StringAttribute{
					Description: "The resource ID of the DeltaStream dataplane (provided by DeltaStream).",
					Required:    true,
				},
				"cluster_index": schema.Int64Attribute{
					Description: "The index of the cluster (provided by DeltaStream).",
					Optional:    true,
				},
				"product_version": schema.StringAttribute{
					Description: "The version of the DeltaStream product. (provided by DeltaStream)",
					Required:    true,
				},
				"cpu_architecture": schema.StringAttribute{
					Description: "The CPU Architecture for EKS.",
					Required:    true,
				},
				"nodepool_instance_types": schema.ListAttribute{
					Description: "The list of instance types for the node pool.",
					ElementType: basetypes.StringType{},
					Required:    true,
				},
				"nodepool_capacity_type": schema.StringAttribute{
					Description: "The capacity type for the node pool, can be on-demand or spot.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.OneOf("spot", "on-demand")},
				},
				"nodepool_cpu_limit": schema.Int32Attribute{
					Description: "The CPU limit for the node pool.",
					Required:    true,
				},
				"vpc_id": schema.StringAttribute{
					Description: "The VPC ID of the cluster.",
					Required:    true,
				},
				"vpc_cidr": schema.StringAttribute{
					Description: "The CIDR of the VPC.",
					Required:    true,
				},
				"vpc_dns_ip": schema.StringAttribute{
					Description: "The VPC DNS server IP address.",
					Required:    true,
					Validators:  []validator.String{},
				},
				"vpn_mode": schema.StringAttribute{
					Description: "The VPN Mode for the cluster, can be None, Tailscale, AwsVpn.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.OneOf("None", "Tailscale", "AwsVpn")},
				},
				"private_link_subnets_ids": schema.ListAttribute{
					Description: "The private subnet IDs of the private links from dataplane VPC.",
					ElementType: basetypes.StringType{},
					Required:    true,
				},

				"private_subnet_ids": schema.ListAttribute{
					Description: "The private subnet IDs hosting nodes for this cluster.",
					ElementType: basetypes.StringType{},
					Required:    true,
					Validators:  []validator.List{listvalidator.SizeAtLeast(2)},
				},
				"public_subnet_ids": schema.ListAttribute{
					Description: "The public subnet IDs with internet gateway.",
					ElementType: basetypes.StringType{},
					Required:    true,
				},
				"metrics_url": schema.StringAttribute{
					Description: "The URL to push metrics.",
					Required:    true,
				},
				"interruption_queue_name": schema.StringAttribute{
					Description: "The name of the SQS queue for handling interruption events.",
					Required:    true,
				},
				"product_artifacts_bucket": schema.StringAttribute{
					Description: "The S3 bucket for storing DeltaStream product artifacts.",
					Required:    true,
				},
				"serde_bucket": schema.StringAttribute{
					Description: "The S3 bucket for storing SERDE artifacts.",
					Required:    true,
				},
				"functions_bucket": schema.StringAttribute{
					Description: "The S3 bucket for storing Functiosn jar artifacts.",
					Required:    true,
				},
				"workload_state_bucket": schema.StringAttribute{
					Description: "The S3 bucket for storing workload state.",
					Required:    true,
				},
				"o11y_bucket": schema.StringAttribute{
					Description: "The S3 bucket for storing observability data.",
					Required:    true,
				},
				"orb_billing_bucket": schema.StringAttribute{
					Description: "The S3 bucket for storing billing data.",
					Required:    true,
				},
				"orb_billing_bucket_region": schema.StringAttribute{
					Description: "The S3 bucket region for storing billing data.",
					Required:    true,
				},
				"aws_secrets_manager_ro_role_arn": schema.StringAttribute{
					Description: "The ARN of the role to assume for reading secrets from AWS secrets manager.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/.+$`), "Invalid Role ARN")},
				},
				"debezium_role_arn": schema.StringAttribute{
					Description: "The ARN of the role to assume for debezium service.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/.+$`), "Invalid Role ARN")},
				},
				"infra_manager_role_arn": schema.StringAttribute{
					Description: "The ARN of the role to assume for managing infra resources.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/.+$`), "Invalid Role ARN")},
				},
				"vault_role_arn": schema.StringAttribute{
					Description: "The ARN of the role to assume for credential vault resources.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/.+$`), "Invalid Role ARN")},
				},
				"vault_init_role_arn": schema.StringAttribute{
					Description: "The ARN of the role to assume for configuring credential vault.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/.+$`), "Invalid Role ARN")},
				},
				"loki_role_arn": schema.StringAttribute{
					Description: "The ARN of the role to assume for managing Loki resources.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/.+$`), "Invalid Role ARN")},
				},
				"tempo_role_arn": schema.StringAttribute{
					Description: "The ARN of the role to assume for managing Tempo resources.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/.+$`), "Invalid Role ARN")},
				},
				"thanos_store_gateway_role_arn": schema.StringAttribute{
					Description: "The ARN of the role to assume for managing Thanos storage gateway resources.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/.+$`), "Invalid Role ARN")},
				},
				"thanos_store_compactor_role_arn": schema.StringAttribute{
					Description: "The ARN of the role to assume for managing Thanos storage compactor resources.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/.+$`), "Invalid Role ARN")},
				},
				"thanos_store_bucket_role_arn": schema.StringAttribute{
					Description: "The ARN of the role to assume for managing Thanos store bucket resources.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/.+$`), "Invalid Role ARN")},
				},
				"thanos_sidecar_role_arn": schema.StringAttribute{
					Description: "The ARN of the role to assume for managing Thanos sidecar resources.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/.+$`), "Invalid Role ARN")},
				},
				"deadman_alert_role_arn": schema.StringAttribute{
					Description: "The ARN of the role to assume for managing deadman alert resources.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/.+$`), "Invalid Role ARN")},
				},
				"karpenter_node_role_name": schema.StringAttribute{
					Description: "The name of the role to assumed by nodes started by Karpenter.",
					Required:    true,
				},
				"karpenter_irsa_role_arn": schema.StringAttribute{
					Description: "The ARN of the role to assume by Karpenter.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/.+$`), "Invalid Role ARN")},
				},
				"store_proxy_role_arn": schema.StringAttribute{
					Description: "The ARN of the role to assume to facilitate connection to customer stores.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/.+$`), "Invalid Role ARN")},
				},
				"cw2loki_role_arn": schema.StringAttribute{
					Description: "The ARN of the role to assume for managing CloudWatch-Loki resources.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/.+$`), "Invalid Role ARN")},
				},
				"ecr_readonly_role_arn": schema.StringAttribute{
					Description: "The ARN of the role to assume for read-only access to ECR.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/.+$`), "Invalid Role ARN")},
				},
				"ds_cross_account_role_arn": schema.StringAttribute{
					Description: "The ARN of the role for provising trust when accessing customer provided resources.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/.+$`), "Invalid Role ARN")},
				},
				"kafka_role_arn": schema.StringAttribute{
					Description: "The ARN of the role to assume for interacting with Kafka topcis and data.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/.+$`), "Invalid Role ARN")},
				},
				"kafka_role_external_id": schema.StringAttribute{
					Description: "The external ID for the kafka role.",
					Required:    true,
				},
				"aws_load_balancer_controller_role_arn": schema.StringAttribute{
					Description: "The ARN of the role to assume for managing AWS Load Balancer resources.",
					Required:    true,
				},
				"additional_api_server_cors_uris": schema.StringAttribute{
					Description: "Addition Web Console URIs for api server CORS setting.",
					Required:    true,
				},

				"workload_credentials_mode": schema.StringAttribute{
					Description: "The mode for managing workload credentials.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.OneOf("secret", "iamrole")},
				},
				"workload_credentials_secret": schema.StringAttribute{
					Description: "The name of the secret containing workload credentials if running in secret mode.",
					Optional:    true,
				},
				"image_build_credentials_secret": schema.StringAttribute{
					Description: "The name of the secret containing image builder credentials.",
					Optional:    true,
				},
				"workload_role_arn": schema.StringAttribute{
					Description: "The ARN of the role to assume for workloads.",
					Optional:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/.+$`), "Invalid Role ARN")},
				},
				"workload_manager_role_arn": schema.StringAttribute{
					Description: "The ARN of the role to assume for managing workloads.",
					Optional:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/.+$`), "Invalid Role ARN")},
				},

				"o11y_hostname": schema.StringAttribute{
					Description: "The hostname of the observability endpoint.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^[a-zA-Z0-9-\.]+\.[a-zA-Z]{2,}$`), "Invalid hostname")},
				},
				"o11y_ingress_security_groups": schema.StringAttribute{
					Description: "Comma separated AWS security group name(s) that will be attached to obervability endpoint load balancer.",
					Optional:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^[a-zA-Z0-9-,]+$`), "Invalid o11y ingress security group names")},
				},
				"o11y_subnet_mode": schema.StringAttribute{
					Description: "The subnet mode for observability endpoint.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.OneOf("public", "private", "none")},
				},
				"o11y_tls_mode": schema.StringAttribute{
					Description: "The TLS/HTTPS mode for observability endpoint.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.OneOf("awscert", "acme", "disabled", "none")},
				},
				"o11y_tls_certificate_arn": schema.StringAttribute{
					Description: "The ARN of the TLS certificate for the observability endpoint.",
					Optional:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws:acm:.+:[0-9]{12}:certificate/.+$`), "Invalid Certificate ARN")},
				},

				"custom_credentials_role_arn": schema.StringAttribute{
					Description: "The ARN of the role to assume for use by the custom credentials plugin.",
					Optional:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/.+$`), "Invalid Role ARN")},
				},
				"custom_credentials_image": schema.StringAttribute{
					Description: "The image to use for the custom credentials plugin.",
					Optional:    true,
				},

				"api_hostname": schema.StringAttribute{
					Description: "The hostname of the dataplane API endpoint.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^[a-zA-Z0-9-\.]+\.[a-zA-Z]{2,}$`), "Invalid hostname")},
				},
				"api_ingress_security_groups": schema.StringAttribute{
					Description: "Comma separated AWS security group name(s) that will be attached to API endpoint load balancer.",
					Optional:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^[a-zA-Z0-9-,]+$`), "Invalid api ingress security group names")},
				},
				"api_subnet_mode": schema.StringAttribute{
					Description: "The subnet mode for dataplane API endpoint.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.OneOf("public", "private")},
				},
				"api_tls_mode": schema.StringAttribute{
					Description: "The TLS/HTTPS mode for dataplane API endpoint.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.OneOf("awscert", "acme", "disabled")},
				},
				"api_tls_certificate_arn": schema.StringAttribute{
					Description: "The ARN of the TLS certificate for the dataplane API endpoint.",
					Optional:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^arn:aws:acm:.+:[0-9]{12}:certificate/.+$`), "Invalid Certificate ARN")},
				},

				"kms_key_id": schema.StringAttribute{
					Description: "The KMS key ID for encrypting credentials store in the dataplane vault.",
					Required:    true,
				},
				"dynamodb_table_name": schema.StringAttribute{
					Description: "The name of the DynamoDB table for storing credentials in the dataplane vault.",
					Required:    true,
				},

				"kafka_hosts": schema.ListAttribute{
					Description: "The list of kafka brokers.",
					ElementType: basetypes.StringType{},
					Required:    true,
				},
				"kafka_listener_ports": schema.ListAttribute{
					Description: "The list of kafka listener ports.",
					ElementType: basetypes.StringType{},
					Required:    true,
				},
				"kafka_cluster_name": schema.StringAttribute{
					Description: "The name of the kafka cluster.",
					Required:    true,
				},

				"rds_control_plane_resource_id": schema.StringAttribute{
					Description: "The resource ID of the RDS controlplane instance for storing DeltaStream data.",
					Required:    true,
				},

				"rds_mviews_resource_id": schema.StringAttribute{
					Description: "The resource ID of the RDS mviews instance for storing Materialized views data.",
					Required:    true,
				},
				"rds_mviews_using_aurora": schema.BoolAttribute{
					Description: "Flag to indicate rds for mviews is aurora cluster.",
					Required:    true,
				},
				"materialized_view_store_type": schema.StringAttribute{
					Description: "Materialized view store type, one of postgres or clickhouse.",
					Required:    true,
					Validators:  []validator.String{stringvalidator.OneOf("postgres", "clickhouse")},
				},
				"cw2loki_sqs_url": schema.StringAttribute{
					Description: "The SQS URL for ingesting CloudWatch data into observability tools.",
					Required:    true,
				},

				"console_hostname": schema.StringAttribute{
					Description: "The hostname of the DeltaStream console",
					Required:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^[a-zA-Z0-9-\.]+\.[a-zA-Z]{2,}$`), "Invalid hostname")},
				},

				"downloads_hostname": schema.StringAttribute{
					Description: "The hostname of the DeltaStream downloads",
					Required:    true,
					Validators:  []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^[a-zA-Z0-9-\.]+\.[a-zA-Z]{2,}$`), "Invalid hostname")},
				},

				"rds_ca_certs_secret": schema.StringAttribute{
					Description: "The secret id in AWS secrets manager holding RDS instance AWS CA certificates",
					Required:    true,
				},

				"rds_control_plane_master_password_secret": schema.StringAttribute{
					Description: "The secret id in AWS secrets manager holding RDS control plane admin credentials managed and rotated by RDS",
					Required:    true,
				},
				"rds_control_plane_database_name": schema.StringAttribute{
					Description: "RDS control plane postgres database name for deltastream",
					Required:    true,
				},
				"rds_control_plane_host_name": schema.StringAttribute{
					Description: "RDS control plane host name",
					Required:    true,
				},
				"rds_control_plane_host_port": schema.Int64Attribute{
					Description: "RDS control plane host name",
					Required:    true,
				},

				"rds_mviews_master_password_secret": schema.StringAttribute{
					Description: "The secret id in AWS secrets manager holding RDS MViews admin credentials managed and rotated by RDS",
					Required:    true,
				},
				"rds_mviews_database_name": schema.StringAttribute{
					Description: "RDS MViews postgres database name for deltastream",
					Optional:    true,
				},
				"rds_mviews_host_name": schema.StringAttribute{
					Description: "RDS MViews host name",
					Optional:    true,
				},
				"rds_mviews_host_port": schema.Int64Attribute{
					Description: "RDS MViews host name",
					Optional:    true,
				},

				"auth0_login_config": schema.StringAttribute{
					Description: "Auth0 Login Config in base64 string format representing options as json, example: {user_pass_enabled: true, sso_enabled: true, oauth_google_enabled: true}",
					Required:    true,
				},

				"installation_timestamp": schema.StringAttribute{
					Description: "Installation timestamp provided by caller.",
					Required:    true,
				},
			},
		},
		"status": schema.SingleNestedAttribute{
			Computed: true,
			Attributes: map[string]schema.Attribute{
				"provider_version": schema.StringAttribute{
					Description: "The version of the DeltaStream provider used to install the dataplane.",
					Computed:    true,
				},
				"product_version": schema.StringAttribute{
					Description: "The version of the DeltaStream product installed on the dataplane.",
					Computed:    true,
				},
				"last_modified": schema.StringAttribute{
					Description: "The time the dataplane was last updated.",
					Computed:    true,
				},
			},
		},
	},
}
