package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"k8s.io/utils/ptr"

	awsconfig "github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/config"
	"github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/util"
)

const deploymentConfigTmpl = `
{
  "vault": {
    "kms": {
      "key_id": "{{ .KmsKeyId }}",
      "region": "{{ .Region }}"
    },
    "dynamodb": {
      "table": "{{ .DynamoDbTable }}",
      "region": "{{ .Region }}"
    }
  },
  "postgres": {
    "username": "deprecated_use_secret",
    "password": "deprecated_use_secret",
    "credentialAwsSecret" : "{{ .RdsCreds.AwsSecretName}}",
    "database": "{{ .RdsConfig.Database }}",
    "sslMode": "verify-full",
    "host": "{{ .RdsConfig.Host }}",
    "port": {{ .RdsConfig.Port }}
  },
  "kafka": {
    "hosts": "{{ .KafkaBrokerList }}",
    "bootstrapBrokersIam": "{{ .KafkaBrokerList }}",
    "brokerListenerPorts": "{{ .KafkaBrokerListenerPorts }}",
    "enableTLS": true,
    "topicReplicas": 3,
    "region": "{{ .Region }}",
    "roleARN": "{{ .KafkaRoleARN }}",
    "externalID": "{{ .KafkaRoleExternalId }}"
  },
  "cpKafka": {
    "hosts": "{{ .KafkaBrokerList }}",
    "bootstrapBrokersIam": "{{ .KafkaBrokerList }}",
    "brokerListenerPorts": "{{ .KafkaBrokerListenerPorts }}",
    "topicReplicas": 3,
    "region": "{{ .Region }}"
  },
  "hostnames": {
    "dpAPIHostname": "{{ .ApiHostname }}"
  },
  "googleOAuth": {
    "clientID": "{{ .DSSecret.GoogleClientID }}",
    "clientSecret": "{{ .DSSecret.GoogleClientSecret }}"
  },
  "s3": {
    "execEngineBucket": {
      "name": "{{ .ProductArtifactsBucket }}",
      "region": "{{ .Region }}",
	  "endpoint": "s3.{{ .Region }}.amazonaws.com",
	  "insecureSkipVerify": false
    },
    "serdeDescriptorBucket": {
      "name": "{{ .SerdeBucket }}",
      "region": "{{ .SerdeBucketRegion }}",
	  "endpoint": "s3.{{ .Region }}.amazonaws.com",
	  "insecureSkipVerify": false
    },
	"functionSrcBucket": {
      "name": "{{ .FunctionsBucket }}",
      "region": "{{ .FunctionsBucketRegion }}",
	  "endpoint": "s3.{{ .Region }}.amazonaws.com",
	  "insecureSkipVerify": false
	},
    "workloadStateBucket": {
      "name": "{{ .WorkloadStateBucket }}",
      "region": "{{ .Region }}",
	  "endpoint": "s3.{{ .Region }}.amazonaws.com",
	  "insecureSkipVerify": false
    },
    "orbBillingBucket": {
      "name": "{{ .OrbBillingBucket }}",
      "region": "{{ .Region }}",
	  "endpoint": "s3.{{ .OrbBillingBucketRegion }}.amazonaws.com",
	  "insecureSkipVerify": false
    },
    "observabilityBucket": {
      "name": "{{ .O11yBucket }}",
      "region": "{{ .Region }}",
	  "endpoint": "",
	  "insecureSkipVerify": false
    },
    "cw2loki": {
      "name": "{{ .O11yBucket }}",
      "region": "{{ .Region }}",
	  "endpoint": "s3.{{ .Region }}.amazonaws.com",
	  "insecureSkipVerify": false,
      "bucket_prefix" : "cw2loki"
    }
  },
  "kube": {
    "storageClass": "gp3"
  },
  "slack": {
    "token": "to_be_deprecated",
    "channel": "to_be_deprecated",
    "pingUser": "to_be_deprecated"
  },
  "pagerduty": {
    "serviceKey": "{{ .DSSecret.PagerdutyServiceKey }}"
  },
  "cw2loki": {
    "eksClusterName": "{{ .KubeClusterName }}",
    "mskClusterName": "{{ .KafkaClusterName }}",
    "rdsName": "{{ .RdsClusterName}}",
    "importBucketAccount": "{{ .AccountID }}",
    "sqsURL": "{{ .Cw2LokiSqsURL }}"
  },
  "postHog": {
    "id": "{{ .DSSecret.PostHogPublicId }}"
  },
  "auth0": {
	"audience": "{{ .DSSecret.Auth0Api.Audience }}",
	"domain": "{{ .DSSecret.Auth0Api.Domain }}",
	"clientId": "{{ .DSSecret.Auth0Api.ClientId }}"
  },
  "auth0cli": {
	"audience": "{{ .DSSecret.Auth0Cli.Audience }}",
	"domain": "{{ .DSSecret.Auth0Cli.Domain }}",
	"clientId": "{{ .DSSecret.Auth0Cli.ClientId }}"
  },
  "sendgrid": {
	"key": "{{ .DSSecret.SendgridApiKey }}"
  },
  "materializedViewPostgres": {
    "username": "deprecated_use_secret",
    "password": "deprecated_use_secret",
    "credentialAwsSecret" : "{{ .MaterializedViewRdsCreds.AwsSecretName}}",
    "database": "{{ .MaterializedViewRdsConfig.Database }}",
    "sslMode": "verify-full",
    "host": "{{ .MaterializedViewRdsConfig.Host }}",
    "port": {{ .RdsConfig.Port }}
  },
  "interactiveKafka": {
    "hosts": "{{ .KafkaBrokerList }}",
    "bootstrapBrokersIam": "{{ .KafkaBrokerList }}",
    "brokerListenerPorts": "{{ .KafkaBrokerListenerPorts }}",
    "enableTLS": true,
    "topicReplicas": 3,
    "region": "{{ .Region }}",
    "roleARN": "{{ .KafkaRoleARN }}",
    "externalID": "{{ .KafkaRoleExternalId }}"
  },
  "tailscale": {
	"clientId": "{{ .DSSecret.Tailscale.ClientId }}",
	"clientSecret": "{{ .DSSecret.Tailscale.ClientSecret }}"
  }
}`

// ^^^ TODO: update materializeViewPostgres to new rds instance
// update interactiveKafka to use separate IAM role arn for interactive topics

type DSSecrets struct {
	GoogleClientID      string    `json:"googleClientID"`
	GoogleClientSecret  string    `json:"googleClientSecret"`
	PagerdutyServiceKey string    `json:"pagerdutyServiceKey"`
	Auth0Api            Auth0     `json:"auth0api"`
	Auth0Cli            Auth0     `json:"auth0cli"`
	SendgridApiKey      string    `json:"sendgridApiKey"`
	PostHogPublicId     string    `json:"posthogPublicID"`
	Tailscale           Tailscale `json:"tailscale"`
}

type Auth0 struct {
	Audience string `json:"audience"`
	Domain   string `json:"domain"`
	ClientId string `json:"clientId"`
}

type PostgresCredSecret struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	AwsSecretName string
}

type PostgresHostConfig struct {
	Host     string
	Port     int
	Database string
}

type Tailscale struct {
	ClientId     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

func UpdateDeploymentConfig(ctx context.Context, cfg aws.Config, dp awsconfig.AWSDataplane) (diags diag.Diagnostics) {
	config, dg := dp.ClusterConfigurationData(ctx)
	diags.Append(dg...)
	if diags.HasError() {
		return
	}

	// Get DeltaStream secret with credentials for PagerDuty, Slack, and Google OAuth
	dsCfg := cfg.Copy()
	dsCfg.Region = config.DsRegion.ValueString()
	dsSecretsmanagerClient := secretsmanager.NewFromConfig(dsCfg)
	providerSecretArn := fmt.Sprintf("%s:secret:deltastream/%s/ds/aws/%s/deployment/%s/provider-orchestration", util.GetARNForCPService(ctx, cfg, config, "secretsmanager"), config.Stack.ValueString(), cfg.Region, config.InfraId.ValueString())
	dsSecret, err := dsSecretsmanagerClient.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: ptr.To(providerSecretArn),
	})
	if err != nil {
		diags.AddError("unable to read DeltaStream secret "+providerSecretArn, err.Error())
		return
	}

	dsSecrets := &DSSecrets{}
	if err := json.Unmarshal([]byte(ptr.Deref(dsSecret.SecretString, string(dsSecret.SecretBinary))), dsSecrets); err != nil {
		diags.AddError("unable to unmarshal DeltaStream secret", err.Error())
		return
	}

	// Get Postgres credentials
	secretsmanagerClient := secretsmanager.NewFromConfig(cfg)
	rdsSecretArn := fmt.Sprintf("%s:secret:%s", util.GetARNForService(ctx, cfg, config, "secretsmanager"), config.RdsMasterPasswordSecret.ValueString())
	rdsCred, err := secretsmanagerClient.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: ptr.To(rdsSecretArn),
	})
	if err != nil {
		diags.AddError("unable to read rds credentials "+rdsSecretArn, err.Error())
		return
	}

	pgCred := &PostgresCredSecret{}
	if err := json.Unmarshal([]byte(ptr.Deref(rdsCred.SecretString, string(rdsCred.SecretBinary))), pgCred); err != nil {
		diags.AddError("unable to unmarshal rds credentials", err.Error())
		return
	}
	pgCred.AwsSecretName = config.RdsMasterPasswordSecret.ValueString()

	rdsConfig := &PostgresHostConfig{
		Host:     config.RdsHostName.ValueString(),
		Port:     int(config.RdsHostPort.ValueInt64()),
		Database: config.RdsDatabaseName.ValueString(),
	}

	// pass materialized View Rds Config
	// note we will separate out the materialized view RDS from control plane RDS to avoid impact of materialized views queries on control plane workflows
	// right now testing with same Rds instance
	materializedViewRdsConfig := &PostgresHostConfig{
		Host:     config.RdsHostName.ValueString(),
		Port:     int(config.RdsHostPort.ValueInt64()),
		Database: config.RdsDatabaseName.ValueString(),
	}
	materializedViewPgCred := &PostgresCredSecret{}
	// note require separate creds for materialized views
	if err := json.Unmarshal([]byte(ptr.Deref(rdsCred.SecretString, string(rdsCred.SecretBinary))), materializedViewPgCred); err != nil {
		diags.AddError("unable to unmarshal rds credentials", err.Error())
		return
	}
	materializedViewPgCred.AwsSecretName = config.RdsMasterPasswordSecret.ValueString()

	tmpl, err := template.New("deploymentConfig").Parse(deploymentConfigTmpl)
	if err != nil {
		diags.AddError("unable to parse deployment config template", err.Error())
		return
	}

	kafkaBrokers := []string{}
	diags.Append(config.KafkaHosts.ElementsAs(ctx, &kafkaBrokers, false)...)
	if diags.HasError() {
		return
	}

	kafkaListenerPorts := []string{}
	diags.Append(config.KafkaListenerPorts.ElementsAs(ctx, &kafkaListenerPorts, false)...)
	if diags.HasError() {
		return
	}

	kubeClusterName, err := util.GetKubeClusterName(ctx, dp)
	if err != nil {
		diags.AddError("unable to get kube cluster name", err.Error())
		return
	}

	rdsClusterName := fmt.Sprintf("ds-%s-%s-%s-db-0", config.InfraId.ValueString(), config.Stack.ValueString(), config.RdsResourceID.ValueString())
	var buf bytes.Buffer
	err = tmpl.Execute(&buf, map[string]any{
		"AccountID":                 config.AccountId.ValueString(),
		"Region":                    cfg.Region,
		"KmsKeyId":                  config.KmsKeyId.ValueString(),
		"DynamoDbTable":             config.DynamoDbTableName.ValueString(),
		"RdsCreds":                  pgCred,
		"RdsConfig":                 rdsConfig,
		"MaterializedViewRdsCreds":  materializedViewPgCred,
		"MaterializedViewRdsConfig": materializedViewRdsConfig,
		"DSSecret":                  dsSecrets,
		"KafkaBrokerList":           strings.Join(kafkaBrokers, ","),
		"KafkaBrokerListenerPorts":  strings.Join(kafkaListenerPorts, ","),
		"KafkaRoleARN":              config.KafkaRoleArn.ValueString(),
		"KafkaRoleExternalId":       config.KafkaRoleExternalId.ValueString(),
		"ApiHostname":               config.ApiHostname.ValueString(),
		"ProductArtifactsBucket":    config.ProductArtifactsBucket.ValueString(),
		"SerdeBucket":               config.SerdeBucket.ValueString(),
		"SerdeBucketRegion":         cfg.Region,
		"FunctionsBucket":           config.FunctionsBucket.ValueString(),
		"FunctionsBucketRegion":     cfg.Region,
		"WorkloadStateBucket":       config.WorkloadStateBucket.ValueString(),
		"O11yBucket":                config.O11yBucket.ValueString(),
		"OrbBillingBucket":          config.OrbBillingBucket.ValueString(),
		"OrbBillingBucketRegion":    config.OrbBillingBucketRegion.ValueString(),
		"KubeClusterName":           kubeClusterName,
		"KafkaClusterName":          config.KafkaClusterName.ValueString(),
		"RdsClusterName":            rdsClusterName,
		"Cw2LokiSqsURL":             config.Cw2LokiSqsUrl.ValueString(),
	})
	if err != nil {
		diags.AddError("unable to render deployment config", err.Error())
		return
	}

	deploymentConfigSecretName := calcDeploymentConfigSecretName(config, cfg.Region)
	if _, err := secretsmanagerClient.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{
		SecretId: aws.String(deploymentConfigSecretName),
	}); err != nil {
		var resourceNotFoundException *types.ResourceNotFoundException
		if errors.As(err, &resourceNotFoundException) {
			if _, err = secretsmanagerClient.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
				Name:         ptr.To(deploymentConfigSecretName),
				SecretString: ptr.To(buf.String()),
				Tags: []types.Tag{
					{Key: ptr.To("deltastream-io-region"), Value: ptr.To(cfg.Region)},
					{Key: ptr.To("deltastream-io-team"), Value: ptr.To("true")},
					{Key: ptr.To("deltastream-io-is-prod"), Value: ptr.To("true")},
					{Key: ptr.To("deltastream-io-env"), Value: ptr.To(config.Stack.ValueString())},
					{Key: ptr.To("deltastream-io-id"), Value: ptr.To(config.InfraId.ValueString())},
					{Key: ptr.To("deltastream-io-name"), Value: ptr.To("ds-" + config.InfraId.ValueString())},
					{Key: ptr.To("deltastream-io-is-byoc"), Value: ptr.To("true")},
				},
			}); err != nil {
				diags.AddError("unable to create deployment config "+deploymentConfigSecretName, err.Error())
				return
			}
		} else {
			diags.AddError("unable to describe deployment config "+deploymentConfigSecretName, err.Error())
			return
		}
	} else {
		deploymentConfigJson := buf.String()
		if !json.Valid([]byte(deploymentConfigJson)) {
			diags.AddError("deployment config validation error", "content must be valid json for "+deploymentConfigSecretName)
			return
		}
		if _, err = secretsmanagerClient.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{
			SecretId:     ptr.To(deploymentConfigSecretName),
			SecretString: ptr.To(buf.String()),
		}); err != nil {
			diags.AddError("unable to write deployment config "+deploymentConfigSecretName, err.Error())
			return
		}
	}

	return
}

func calcDeploymentConfigSecretName(config awsconfig.ClusterConfiguration, region string) string {
	return fmt.Sprintf("deltastream/%s/ds/%s/aws/%s/%s/deployment-config", config.Stack.ValueString(), config.InfraId.ValueString(), region, config.EksResourceId.ValueString())
}
