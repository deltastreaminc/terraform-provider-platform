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
	"github.com/aws/aws-sdk-go-v2/service/rds"
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
    "credentialAwsSecret" : "{{ .RdsControlPlaneCreds.AwsSecretName}}",
    "database": "{{ .RdsControlPlaneConfig.Database }}",
    "sslMode": "verify-full",
    "host": "{{ .RdsControlPlaneConfig.Host }}",
    "port": {{ .RdsControlPlaneConfig.Port }}
  },
  "kafka": {
    "hosts": "{{ .KafkaBrokerList }}",
    "bootstrapBrokersIam": "{{ .KafkaBrokerList }}",
    "brokerListenerPorts": "{{ .KafkaBrokerListenerPorts }}",
    "enableTLS": true,
    "topicReplicas": {{ .TopicReplicas }},
    "region": "{{ .Region }}",
    "roleARN": "{{ .KafkaRoleARN }}",
    "externalID": "{{ .KafkaRoleExternalId }}"
  },
  "cpKafka": {
    "hosts": "{{ .KafkaBrokerList }}",
    "bootstrapBrokersIam": "{{ .KafkaBrokerList }}",
    "brokerListenerPorts": "{{ .KafkaBrokerListenerPorts }}",
    "topicReplicas": {{ .TopicReplicas }},
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
    "rdsName": "{{ .RdsControlPlaneClusterName}}",
    "importBucketAccount": "{{ .AccountID }}",
    "sqsURL": "{{ .Cw2LokiSqsURL }}"
  },
  "postHog": {
    "id": "{{ .DSSecret.PostHogPublicId }}"
  },
  "auth0": {
	"audience": "{{ .DSSecret.Auth0Api.Audience }}",
	"domain": "{{ .DSSecret.Auth0Api.Domain }}",
	"clientId": "{{ .DSSecret.Auth0Api.ClientId }}",
	"loginConfig": "{{ .Auth0LoginConfig }}"
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
    "port": {{ .MaterializedViewRdsConfig.Port }}
  },
  "interactiveKafka": {
    "hosts": "{{ .KafkaBrokerList }}",
    "bootstrapBrokersIam": "{{ .KafkaBrokerList }}",
    "brokerListenerPorts": "{{ .KafkaBrokerListenerPorts }}",
    "enableTLS": true,
    "topicReplicas": {{ .TopicReplicas }},
    "region": "{{ .Region }}",
    "roleARN": "{{ .KafkaRoleARN }}",
    "externalID": "{{ .KafkaRoleExternalId }}"
  },
  "tailscale": {
	"clientId": "{{ .DSSecret.Tailscale.ClientId }}",
	"clientSecret": "{{ .DSSecret.Tailscale.ClientSecret }}"
  },
  "trialConfig": {
    "trialStoreRegion": "{{ .DSSecret.TrialConfig.TrialStoreRegion }}",
    "trialStoreSQSUrl": "{{ .DSSecret.TrialConfig.TrialStoreSQSUrl }}",
    "trialStoreSQSRegion": "{{ .DSSecret.TrialConfig.TrialStoreSQSRegion }}",
    "trialStoreKafkaUri": "{{ .DSSecret.TrialConfig.TrialStoreKafkaUri }}",
    "trialStoreHashFunction": "{{ .DSSecret.TrialConfig.TrialStoreHashFunction }}"
  }
}`

type DSSecrets struct {
	GoogleClientID      string      `json:"googleClientID"`
	GoogleClientSecret  string      `json:"googleClientSecret"`
	PagerdutyServiceKey string      `json:"pagerdutyServiceKey"`
	Auth0Api            Auth0       `json:"auth0api"`
	Auth0Cli            Auth0       `json:"auth0cli"`
	SendgridApiKey      string      `json:"sendgridApiKey"`
	PostHogPublicId     string      `json:"posthogPublicID"`
	Tailscale           Tailscale   `json:"tailscale"`
	TrialConfig         TrialConfig `json:"trialConfig"`
}

type TrialConfig struct {
	TrialStoreRegion       string `json:"trialStoreRegion"`
	TrialStoreSQSUrl       string `json:"trialStoreSQSUrl"`
	TrialStoreSQSRegion    string `json:"trialStoreSQSRegion"`
	TrialStoreKafkaUri     string `json:"trialStoreKafkaUri"`
	TrialStoreHashFunction string `json:"trialStoreHashFunction"`
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

	// Get Control plane Postgres credentials
	secretsmanagerClient := secretsmanager.NewFromConfig(cfg)
	rdsControlPlaneSecretArn := fmt.Sprintf("%s:secret:%s", util.GetARNForService(ctx, cfg, config, "secretsmanager"), config.RdsControlPlaneMasterPasswordSecret.ValueString())
	rdsControlPlaneCred, err := secretsmanagerClient.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: ptr.To(rdsControlPlaneSecretArn),
	})
	if err != nil {
		diags.AddError("unable to read rds credentials "+rdsControlPlaneSecretArn, err.Error())
		return
	}

	pgControlPlaneCred := &PostgresCredSecret{}
	if err := json.Unmarshal([]byte(ptr.Deref(rdsControlPlaneCred.SecretString, string(rdsControlPlaneCred.SecretBinary))), pgControlPlaneCred); err != nil {
		diags.AddError("unable to unmarshal rds credentials", err.Error())
		return
	}
	pgControlPlaneCred.AwsSecretName = config.RdsControlPlaneMasterPasswordSecret.ValueString()

	rdsControlPlaneConfig := &PostgresHostConfig{
		Host:     config.RdsControlPlaneHostName.ValueString(),
		Port:     int(config.RdsControlPlaneHostPort.ValueInt64()),
		Database: config.RdsControlPlaneDatabaseName.ValueString(),
	}

	// Get MViews Postgres credentials
	rdsMViewsSecretArn := fmt.Sprintf("%s:secret:%s", util.GetARNForService(ctx, cfg, config, "secretsmanager"), config.RdsMViewsMasterPasswordSecret.ValueString())
	rdsMViewsCred, err := secretsmanagerClient.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: ptr.To(rdsMViewsSecretArn),
	})
	if err != nil {
		diags.AddError("unable to read rds credentials "+rdsControlPlaneSecretArn, err.Error())
		return
	}

	mviewPGHostname := config.RdsMViewsHostName.ValueString()
	mviewPGPort := int(config.RdsMViewsHostPort.ValueInt64())
	mviewDBName := config.RdsMViewsDatabaseName.ValueString()
	if config.RdsMViewsUsingAurora.ValueBool() {
		// Terraform does not provide a data resource to lookup an RDS aurora cluster configuration
		// Use AWS RDS client to identify dbname, endpoints and ports
		rdsClient := rds.NewFromConfig(cfg)

		rdsMViewAuroraClusterName := fmt.Sprintf("ds-%s-%s-%s-db-0", config.InfraId.ValueString(), config.Stack.ValueString(), config.RdsMViewsResourceID.ValueString())

		dbClusterInput := &rds.DescribeDBClustersInput{
			DBClusterIdentifier: aws.String(rdsMViewAuroraClusterName),
		}

		dbCluster, err := rdsClient.DescribeDBClusters(ctx, dbClusterInput)
		if err != nil {
			diags.AddError("failed to describe DB clusters "+rdsMViewAuroraClusterName, err.Error())
		}
		mviewDBName = ""
		mviewPGHostname = ""
		mviewPGPort = 0

		for _, cluster := range dbCluster.DBClusters {
			mviewDBName = *cluster.DatabaseName
			mviewPGPort = int(*cluster.Port)
			// for now pass the single database name for mviews
			break
		}

		if mviewDBName == "" {
			diags.AddError("failed to get database name for Aurora DB cluster "+rdsMViewAuroraClusterName, err.Error())
			return
		}

		auroraClusterEPInput := &rds.DescribeDBClusterEndpointsInput{
			DBClusterIdentifier: aws.String(rdsMViewAuroraClusterName),
		}

		// use describe cluster endpoint to identify mview host name
		auroraClusterEP, err := rdsClient.DescribeDBClusterEndpoints(ctx, auroraClusterEPInput)
		if err != nil {
			diags.AddError("failed to describe Aurora DB cluster endpoints for "+rdsMViewAuroraClusterName, err.Error())
			return
		}

		for _, endpoint := range auroraClusterEP.DBClusterEndpoints {
			if strings.ToUpper(*endpoint.EndpointType) == "WRITER" {
				mviewPGHostname = *endpoint.Endpoint
				break
			}
		}

		if mviewPGHostname == "" {
			diags.AddError("failed to get WRITER Endpoint for Aurora DB cluster "+rdsMViewAuroraClusterName, err.Error())
			return
		}
	}
	// materialized View Rds Config
	materializedViewRdsConfig := &PostgresHostConfig{
		Host:     mviewPGHostname,
		Port:     mviewPGPort,
		Database: mviewDBName,
	}
	materializedViewPgCred := &PostgresCredSecret{}
	// note require separate creds for materialized views
	if err := json.Unmarshal([]byte(ptr.Deref(rdsMViewsCred.SecretString, string(rdsMViewsCred.SecretBinary))), materializedViewPgCred); err != nil {
		diags.AddError("unable to unmarshal rds credentials", err.Error())
		return
	}
	materializedViewPgCred.AwsSecretName = config.RdsMViewsMasterPasswordSecret.ValueString()

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
	topicReplicas := 3

	clusterSubnetIds := []string{}
	d.Append(config.PrivateSubnetIds.ElementsAs(ctx, &clusterSubnetIds, false)...)
	if d.HasError() {
		return
	}
	if len(clusterSubnetIds) < topicReplicas {
		topicReplicas := len(clusterSubnetIds)
	}

	rdsControlPlaneClusterName := fmt.Sprintf("ds-%s-%s-%s-db-0", config.InfraId.ValueString(), config.Stack.ValueString(), config.RdsControlPlaneResourceID.ValueString())
	var buf bytes.Buffer
	err = tmpl.Execute(&buf, map[string]any{
		"AccountID":                  config.AccountId.ValueString(),
		"Region":                     cfg.Region,
		"TopicReplicas":              topicReplicas,
		"KmsKeyId":                   config.KmsKeyId.ValueString(),
		"DynamoDbTable":              config.DynamoDbTableName.ValueString(),
		"RdsControlPlaneCreds":       pgControlPlaneCred,
		"RdsControlPlaneConfig":      rdsControlPlaneConfig,
		"MaterializedViewRdsCreds":   materializedViewPgCred,
		"MaterializedViewRdsConfig":  materializedViewRdsConfig,
		"Auth0LoginConfig":           config.Auth0LoginConfig,
		"DSSecret":                   dsSecrets,
		"KafkaBrokerList":            strings.Join(kafkaBrokers, ","),
		"KafkaBrokerListenerPorts":   strings.Join(kafkaListenerPorts, ","),
		"KafkaRoleARN":               config.KafkaRoleArn.ValueString(),
		"KafkaRoleExternalId":        config.KafkaRoleExternalId.ValueString(),
		"ApiHostname":                config.ApiHostname.ValueString(),
		"ProductArtifactsBucket":     config.ProductArtifactsBucket.ValueString(),
		"SerdeBucket":                config.SerdeBucket.ValueString(),
		"SerdeBucketRegion":          cfg.Region,
		"FunctionsBucket":            config.FunctionsBucket.ValueString(),
		"FunctionsBucketRegion":      cfg.Region,
		"WorkloadStateBucket":        config.WorkloadStateBucket.ValueString(),
		"O11yBucket":                 config.O11yBucket.ValueString(),
		"OrbBillingBucket":           config.OrbBillingBucket.ValueString(),
		"OrbBillingBucketRegion":     config.OrbBillingBucketRegion.ValueString(),
		"KubeClusterName":            kubeClusterName,
		"KafkaClusterName":           config.KafkaClusterName.ValueString(),
		"RdsControlPlaneClusterName": rdsControlPlaneClusterName,
		"Cw2LokiSqsURL":              config.Cw2LokiSqsUrl.ValueString(),
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
