---
# generated by https://github.com/hashicorp/terraform-plugin-docs
page_title: "platform_aws Resource - platform"
subcategory: ""
description: |-
  AWS Dataplane resource
---

# platform_aws (Resource)

AWS Dataplane resource



<!-- schema generated by tfplugindocs -->
## Schema

### Required

- `assume_role` (Attributes) Assume role configuration (see [below for nested schema](#nestedatt--assume_role))
- `configuration` (Attributes) Cluster configuration (see [below for nested schema](#nestedatt--configuration))

### Read-Only

- `status` (Attributes) (see [below for nested schema](#nestedatt--status))

<a id="nestedatt--assume_role"></a>
### Nested Schema for `assume_role`

Optional:

- `region` (String) The AWS region to use for the assume role.
- `role_arn` (String) Amazon Resource Name (ARN) of an IAM Role to assume prior to making API calls.
- `session_name` (String) An identifier for the assumed role session.


<a id="nestedatt--configuration"></a>
### Nested Schema for `configuration`

Required:

- `account_id` (String) The account ID hosting the DeltaStream dataplane.
- `additional_api_server_cors_uris` (String) Addition Web Console URIs for api server CORS setting.
- `api_hostname` (String) The hostname of the dataplane API endpoint.
- `api_subnet_mode` (String) The subnet mode for dataplane API endpoint.
- `api_tls_mode` (String) The TLS/HTTPS mode for dataplane API endpoint.
- `auth0_login_config` (String) Auth0 Login Config in base64 string format representing options as json, example: {user_pass_enabled: true, sso_enabled: true, oauth_google_enabled: true}
- `aws_load_balancer_controller_role_arn` (String) The ARN of the role to assume for managing AWS Load Balancer resources.
- `aws_secrets_manager_ro_role_arn` (String) The ARN of the role to assume for reading secrets from AWS secrets manager.
- `console_hostname` (String) The hostname of the DeltaStream console
- `cpu_architecture` (String) The CPU Architecture for EKS.
- `cw2loki_role_arn` (String) The ARN of the role to assume for managing CloudWatch-Loki resources.
- `cw2loki_sqs_url` (String) The SQS URL for ingesting CloudWatch data into observability tools.
- `deadman_alert_role_arn` (String) The ARN of the role to assume for managing deadman alert resources.
- `debezium_role_arn` (String) The ARN of the role to assume for debezium service.
- `downloads_hostname` (String) The hostname of the DeltaStream downloads
- `ds_account_id` (String) The account ID provided by DeltaStream.
- `ds_cross_account_role_arn` (String) The ARN of the role for provising trust when accessing customer provided resources.
- `dynamodb_table_name` (String) The name of the DynamoDB table for storing credentials in the dataplane vault.
- `ecr_readonly_role_arn` (String) The ARN of the role to assume for read-only access to ECR.
- `eks_resource_id` (String) The resource ID of the DeltaStream dataplane (provided by DeltaStream).
- `enable_schema_migration_test` (Boolean) Flag to enable schema migration test before deploying new version.
- `functions_bucket` (String) The S3 bucket for storing Functiosn jar artifacts.
- `infra_id` (String) The infra ID of the DeltaStream dataplane (provided by DeltaStream).
- `infra_manager_role_arn` (String) The ARN of the role to assume for managing infra resources.
- `infra_type` (String) The infra Type for DeltaStream dataplane.
- `installation_timestamp` (String) Installation timestamp provided by caller.
- `interruption_queue_name` (String) The name of the SQS queue for handling interruption events.
- `kafka_cluster_name` (String) The name of the kafka cluster.
- `kafka_hosts` (List of String) The list of kafka brokers.
- `kafka_listener_ports` (List of String) The list of kafka listener ports.
- `kafka_role_arn` (String) The ARN of the role to assume for interacting with Kafka topcis and data.
- `kafka_role_external_id` (String) The external ID for the kafka role.
- `karpenter_irsa_role_arn` (String) The ARN of the role to assume by Karpenter.
- `karpenter_node_role_name` (String) The name of the role to assumed by nodes started by Karpenter.
- `kms_key_id` (String) The KMS key ID for encrypting credentials store in the dataplane vault.
- `loki_role_arn` (String) The ARN of the role to assume for managing Loki resources.
- `materialized_view_store_type` (String) Materialized view store type, one of postgres or clickhouse.
- `metrics_url` (String) The URL to push metrics.
- `nodepool_capacity_type` (String) The capacity type for the node pool, can be on-demand or spot.
- `nodepool_cpu_limit` (Number) The CPU limit for the node pool.
- `nodepool_instance_types` (List of String) The list of instance types for the node pool.
- `o11y_bucket` (String) The S3 bucket for storing observability data.
- `o11y_hostname` (String) The hostname of the observability endpoint.
- `o11y_subnet_mode` (String) The subnet mode for observability endpoint.
- `o11y_tls_mode` (String) The TLS/HTTPS mode for observability endpoint.
- `orb_billing_bucket` (String) The S3 bucket for storing billing data.
- `orb_billing_bucket_region` (String) The S3 bucket region for storing billing data.
- `private_link_subnets_ids` (List of String) The private subnet IDs of the private links from dataplane VPC.
- `private_subnet_ids` (List of String) The private subnet IDs hosting nodes for this cluster.
- `product_artifacts_bucket` (String) The S3 bucket for storing DeltaStream product artifacts.
- `product_version` (String) The version of the DeltaStream product. (provided by DeltaStream)
- `public_subnet_ids` (List of String) The public subnet IDs with internet gateway.
- `query_service_role_arn` (String) The ARN of the role to assume for query service.
- `rds_ca_certs_secret` (String) The secret id in AWS secrets manager holding RDS instance AWS CA certificates
- `rds_control_plane_database_name` (String) RDS control plane postgres database name for deltastream
- `rds_control_plane_host_name` (String) RDS control plane host name
- `rds_control_plane_host_port` (Number) RDS control plane host name
- `rds_control_plane_master_password_secret` (String) The secret id in AWS secrets manager holding RDS control plane admin credentials managed and rotated by RDS
- `rds_control_plane_resource_id` (String) The resource ID of the RDS controlplane instance for storing DeltaStream data.
- `rds_mviews_master_password_secret` (String) The secret id in AWS secrets manager holding RDS MViews admin credentials managed and rotated by RDS
- `rds_mviews_resource_id` (String) The resource ID of the RDS mviews instance for storing Materialized views data.
- `rds_mviews_using_aurora` (Boolean) Flag to indicate rds for mviews is aurora cluster.
- `serde_bucket` (String) The S3 bucket for storing SERDE artifacts.
- `store_proxy_role_arn` (String) The ARN of the role to assume to facilitate connection to customer stores.
- `tempo_role_arn` (String) The ARN of the role to assume for managing Tempo resources.
- `thanos_sidecar_role_arn` (String) The ARN of the role to assume for managing Thanos sidecar resources.
- `thanos_store_bucket_role_arn` (String) The ARN of the role to assume for managing Thanos store bucket resources.
- `thanos_store_compactor_role_arn` (String) The ARN of the role to assume for managing Thanos storage compactor resources.
- `thanos_store_gateway_role_arn` (String) The ARN of the role to assume for managing Thanos storage gateway resources.
- `vault_init_role_arn` (String) The ARN of the role to assume for configuring credential vault.
- `vault_role_arn` (String) The ARN of the role to assume for credential vault resources.
- `vpc_cidr` (String) The CIDR of the VPC.
- `vpc_dns_ip` (String) The VPC DNS server IP address.
- `vpc_id` (String) The VPC ID of the cluster.
- `vpn_mode` (String) The VPN Mode for the cluster, can be None, Tailscale, AwsVpn.
- `workload_credentials_mode` (String) The mode for managing workload credentials.
- `workload_state_bucket` (String) The S3 bucket for storing workload state.

Optional:

- `api_ingress_security_groups` (String) Comma separated AWS security group name(s) that will be attached to API endpoint load balancer.
- `api_tls_certificate_arn` (String) The ARN of the TLS certificate for the dataplane API endpoint.
- `cluster_index` (Number) The index of the cluster (provided by DeltaStream).
- `custom_credentials_image` (String) The image to use for the custom credentials plugin.
- `custom_credentials_role_arn` (String) The ARN of the role to assume for use by the custom credentials plugin.
- `ds_region` (String) The AWS region provided by DeltaStream.
- `image_build_credentials_secret` (String) The name of the secret containing image builder credentials.
- `o11y_ingress_security_groups` (String) Comma separated AWS security group name(s) that will be attached to obervability endpoint load balancer.
- `o11y_tls_certificate_arn` (String) The ARN of the TLS certificate for the observability endpoint.
- `rds_mviews_database_name` (String) RDS MViews postgres database name for deltastream
- `rds_mviews_host_name` (String) RDS MViews host name
- `rds_mviews_host_port` (Number) RDS MViews host name
- `stack` (String) The type of DeltaStream dataplane (default: prod).
- `workload_credentials_secret` (String) The name of the secret containing workload credentials if running in secret mode.
- `workload_manager_role_arn` (String) The ARN of the role to assume for managing workloads.
- `workload_role_arn` (String) The ARN of the role to assume for workloads.


<a id="nestedatt--status"></a>
### Nested Schema for `status`

Read-Only:

- `last_modified` (String) The time the dataplane was last updated.
- `product_version` (String) The version of the DeltaStream product installed on the dataplane.
- `provider_version` (String) The version of the DeltaStream provider used to install the dataplane.
