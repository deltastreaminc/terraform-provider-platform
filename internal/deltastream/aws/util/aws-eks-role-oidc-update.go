package util

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// This module contains various resource policy update utility functions, covering s3, ecr, secretsmanager, kms resources
// these help in updating permissions to a shared/multi-tenant resource policy such that add/update/remove of a tenant specific policy does not impact other existing policies

// AddOrUpdateBucketPermissions
// AWS s3 utiltity functions to update S3 bucket policy statement with a statement block without impacting other statements
// this helps in adding tenant specific permission to a management plane bucket
// Important !!! Pass a unique permissionName to ensure that a specific permission will not impact any other existing permission
// permissionName should be compliant with AWS StatementId (Sid) field without special characters or spaces
/* example kmsActions for general s3 Get and Put include following
"kms:Encrypt",
"kms:Decrypt",
"kms:ReEncrypt*",
"kms:GenerateDataKey*",
"kms:DescribeKey"

Example s3Actions for read include following

s3:GetObject*
s3:ListBucket

Example s3ResourcePrefix can be * to allow all resources in bucket or can be a tenant specific prefix (e.g. using unique infra id) to restrict access

*/
func AddOrUpdateBucketPermissions(ctx context.Context, cfg aws.Config, permissionName string, s3Actions []string, s3ResourcePrefix string, kmsActions []string, bucketName, bucketRegion, bucketEndpoint string, allowedPrincipals []string) error {
	log := log.FromContext(ctx).WithName("AddOrUpdateBucketPermissions")

	s3Client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if bucketEndpoint != "" {
			o.BaseEndpoint = aws.String(bucketEndpoint)
		}
		if bucketRegion != "" {
			o.Region = bucketRegion
		}
	})
	bucketPolicy, err := s3Client.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{
		Bucket: ptr.To(bucketName),
	})

	if err != nil {
		return fmt.Errorf("failed to get bucket policy for bucket %s: %w", bucketName, err)
	}

	policy := *bucketPolicy.Policy

	var policyJson map[string]interface{}
	// unmarshal as any Json as statements data structure can vary and using a go object type can cause a loss on a re-marshall
	json.Unmarshal([]byte(policy), &policyJson)
	statements := policyJson["Statement"].([]interface{})

	foundPerms := false
	permsStmtIndex := -1
	for idx, st := range statements {
		stMap := st.(map[string]interface{})
		if _, okst := stMap["Sid"]; okst {
			if stMap["Sid"].(string) == permissionName {
				foundPerms = true
				permsStmtIndex = idx
				break
			}
		}
	}

	var permissionsToUpdate map[string]interface{}
	updateRequired := false
	sort.Strings(s3Actions)
	if !foundPerms {
		quotedActions := convertToQuotedString(s3Actions)
		addPermissionText := fmt.Sprintf(`{
					"Sid": "%s",
		            "Effect": "Allow",
		            "Principal": {
		                "AWS": []
		            },
		            "Action": [ %s ],
		            "Resource": [
		                "arn:aws:s3:::%s",
		                "arn:aws:s3:::%s/%s"
		            ]
				}`, permissionName, strings.Join(quotedActions, ","), bucketName, bucketName, s3ResourcePrefix)

		var addPermissions map[string]interface{}
		json.Unmarshal([]byte(addPermissionText), &addPermissions)

		policyJson["Statement"] = append(statements, addPermissions)
		permissionsToUpdate = addPermissions
		updateRequired = true
	} else {
		permissionsToUpdate = statements[permsStmtIndex].(map[string]interface{})
		existingActions := permissionsToUpdate["Action"].([]string)
		sort.Strings(existingActions)

		permissionsToUpdate["Action"] = s3Actions
		if len(existingActions) != len(s3Actions) || strings.Join(existingActions, ",") != strings.Join(s3Actions, ",") {
			updateRequired = true
		}
	}

	for _, principal := range allowedPrincipals {
		existingPrincipal, arrayOk := permissionsToUpdate["Principal"].(map[string]interface{})["AWS"].([]interface{})
		if !arrayOk {
			// aws changes a single principal to a non array element
			singlePrincipal, singleOk := permissionsToUpdate["Principal"].(map[string]interface{})["AWS"].(string)
			if !singleOk {
				return fmt.Errorf("Invalid policy document found for bucket %s", bucketName)
			}
			existingPrincipal = []interface{}{singlePrincipal}
		}
		foundPrincipal := false
		for _, s := range existingPrincipal {
			if s == principal {
				foundPrincipal = true
				break
			}
		}
		if !foundPrincipal {
			permissionsToUpdate["Principal"].(map[string]interface{})["AWS"] = append(existingPrincipal, principal)
			updateRequired = true
		}
	}

	if updateRequired {
		b, err := json.MarshalIndent(policyJson, "", "   ")
		if err != nil {
			return fmt.Errorf("unable to marshal updated policy json, bucket %s: %w", bucketName, err)
		}
		if _, err := s3Client.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
			Bucket: ptr.To(bucketName),
			Policy: ptr.To(string(b)),
		}); err != nil {
			return fmt.Errorf("unable to update bucket policy for bucket %s: policy %s: %w", bucketName, string(b), err)
		}
		log.Info("bucket permissions updated", "bucket", bucketName, "newPermissions", string(b))
	}

	// check if bucket is using a custom managed kms key
	bucketEncryption, err := s3Client.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{
		Bucket: ptr.To(bucketName),
	})
	if err != nil {
		return fmt.Errorf("unable to get bucket encryption kms key for bucket %s: %w", bucketName, err)
	}
	if len(bucketEncryption.ServerSideEncryptionConfiguration.Rules) > 0 {
		for _, rule := range bucketEncryption.ServerSideEncryptionConfiguration.Rules {
			if rule.ApplyServerSideEncryptionByDefault.SSEAlgorithm == types.ServerSideEncryptionAwsKms && rule.ApplyServerSideEncryptionByDefault.KMSMasterKeyID != nil {
				return UpdateKmsKeyPermission(ctx, cfg, fmt.Sprintf("%sKms", permissionName), kmsActions, *rule.ApplyServerSideEncryptionByDefault.KMSMasterKeyID, allowedPrincipals)
			}
		}
	}
	return nil
}

func RemoveBucketPermissions(ctx context.Context, cfg aws.Config, permissionName, bucketName, bucketRegion, bucketEndpoint string, removePrincipals []string) error {
	log := log.FromContext(ctx).WithName("RemoveBucketPermissions")
	s3Client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if bucketEndpoint != "" {
			o.BaseEndpoint = aws.String(bucketEndpoint)
		}
		if bucketRegion != "" {
			o.Region = bucketRegion
		}
	})
	bucketPolicy, err := s3Client.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{
		Bucket: ptr.To(bucketName),
	})

	if err != nil {
		return fmt.Errorf("failed to get bucket policy for bucket %s: %w", bucketName, err)
	}

	policy := *bucketPolicy.Policy

	var policyJson map[string]interface{}
	// unmarshal as any Json as statements data structure can vary and using a go object type can cause a loss on a re-marshall
	json.Unmarshal([]byte(policy), &policyJson)
	statements := policyJson["Statement"].([]interface{})

	foundPerms := false
	updateRequired := false
	permsStmtIndex := -1
	for idx, st := range statements {
		stMap := st.(map[string]interface{})
		if _, okst := stMap["Sid"]; okst {
			if stMap["Sid"].(string) == permissionName {
				foundPerms = true
				permsStmtIndex = idx
				break
			}
		}
	}

	if foundPerms {
		var permissionsToUpdate map[string]interface{}
		permissionsToUpdate = statements[permsStmtIndex].(map[string]interface{})
		removePermsStatement := false
		for _, principal := range removePrincipals {

			existingPrincipal, arrayOk := permissionsToUpdate["Principal"].(map[string]interface{})["AWS"].([]interface{})
			if !arrayOk {
				singlePrincipal, singleOk := permissionsToUpdate["Principal"].(map[string]interface{})["AWS"].(string)
				if !singleOk {
					return fmt.Errorf("invalid policy document found for bucket %s", bucketName)
				}
				existingPrincipal = []interface{}{singlePrincipal}
			}

			foundPrincipal := false
			foundIndex := -1
			for i, s := range existingPrincipal {
				if s == principal {
					foundPrincipal = true
					foundIndex = i
					break
				}
			}
			if foundPrincipal {
				updateRequired = true
				// remove statement block, if we had a single entry left for removal
				if len(existingPrincipal) <= 1 {
					permissionsToUpdate["Principal"].(map[string]interface{})["AWS"] = []interface{}{}
					removePermsStatement = true
					break
				} else {
					permissionsToUpdate["Principal"].(map[string]interface{})["AWS"] = append(existingPrincipal[:foundIndex], existingPrincipal[foundIndex+1:]...)
				}
			}
		}

		if removePermsStatement {
			//remove permission statement if all perms have been removed, there is an edge case of not having any other statement present in policy, but there is likely a static policy statement present outside of dynamically added policy statements
			policyJson["Statement"] = append(statements[:permsStmtIndex], statements[permsStmtIndex+1:]...)
		}

		if updateRequired {
			b, err := json.MarshalIndent(policyJson, "", "   ")
			if err != nil {
				return fmt.Errorf("unable to marshal updated policy json after removal of principals, bucket %s: %w", bucketName, err)
			}
			if _, err := s3Client.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
				Bucket: ptr.To(bucketName),
				Policy: ptr.To(string(b)),
			}); err != nil {
				return fmt.Errorf("unable to update bucket policy for bucket %s: policy %s: %w", bucketName, string(b), err)
			}
			log.Info("bucket permissions updated after removing principals", "bucket", bucketName, "newPermissions", string(b))
		}
	}
	// check if bucket is using a custom managed kms key
	bucketEncryption, err := s3Client.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{
		Bucket: ptr.To(bucketName),
	})
	if err != nil {
		return fmt.Errorf("unable to get bucket encryption kms key for bucket %s: %w", bucketName, err)
	}
	if len(bucketEncryption.ServerSideEncryptionConfiguration.Rules) > 0 {
		for _, rule := range bucketEncryption.ServerSideEncryptionConfiguration.Rules {
			if rule.ApplyServerSideEncryptionByDefault.SSEAlgorithm == types.ServerSideEncryptionAwsKms && rule.ApplyServerSideEncryptionByDefault.KMSMasterKeyID != nil {
				return RemoveKmsKeyPermission(ctx, cfg, fmt.Sprintf("%sKms", permissionName), *rule.ApplyServerSideEncryptionByDefault.KMSMasterKeyID, removePrincipals)
			}
		}
	}
	return nil
}

func UpdateKmsKeyPermission(ctx context.Context, cfg aws.Config, permissionName string, kmsActions []string, kmsKeyArn string, allowedPrincipals []string) error {
	log := log.FromContext(ctx).WithName("UpdateKmsKeyPermission")

	kmsSvc := kms.NewFromConfig(cfg)

	keyPolicy, err := kmsSvc.GetKeyPolicy(ctx, &kms.GetKeyPolicyInput{
		KeyId: ptr.To(kmsKeyArn),
	})

	if err != nil {
		return fmt.Errorf("failed to get kms key policy, key arn %s: %w", kmsKeyArn, err)
	}
	policy := *keyPolicy.Policy

	var policyJson map[string]interface{}
	// unmarshal as any Json as statements data structure can vary and using a go object type can cause a loss on a re-marshall
	json.Unmarshal([]byte(policy), &policyJson)
	statements := policyJson["Statement"].([]interface{})

	foundPerms := false
	permsStmtIndex := -1
	for idx, st := range statements {
		stMap := st.(map[string]interface{})
		if _, okst := stMap["Sid"]; okst {
			if stMap["Sid"].(string) == permissionName {
				foundPerms = true
				permsStmtIndex = idx
				break
			}
		}
	}

	var permissionsToUpdate map[string]interface{}
	updateRequired := false
	sort.Strings(kmsActions)
	quotedActions := convertToQuotedString(kmsActions)

	if !foundPerms {
		addPermissionText := fmt.Sprintf(`{
			"Sid": "%s",
            "Effect": "Allow",
            "Principal": {
                "AWS": []
            },
            "Action": [%s],
            "Resource": "*"
		}`, permissionName, strings.Join(quotedActions, ","))
		var addPermissions map[string]interface{}
		json.Unmarshal([]byte(addPermissionText), &addPermissions)

		policyJson["Statement"] = append(statements, addPermissions)
		permissionsToUpdate = addPermissions
		updateRequired = true
	} else {
		permissionsToUpdate = statements[permsStmtIndex].(map[string]interface{})
		existingActions := permissionsToUpdate["Action"].([]string)

		permissionsToUpdate["Action"] = kmsActions
		sort.Strings(existingActions)
		if len(existingActions) != len(kmsActions) || strings.Join(kmsActions, ",") != strings.Join(kmsActions, ",") {
			updateRequired = true
		}
	}

	for _, principal := range allowedPrincipals {
		existingPrincipal, arrayOk := permissionsToUpdate["Principal"].(map[string]interface{})["AWS"].([]interface{})
		if !arrayOk {
			// aws changes a single principal to a non array element
			singlePrincipal, singleOk := permissionsToUpdate["Principal"].(map[string]interface{})["AWS"].(string)
			if !singleOk {
				return fmt.Errorf("Invalid policy document found for kms key %s", kmsKeyArn)
			}
			existingPrincipal = []interface{}{singlePrincipal}
		}
		foundPrincipal := false
		for _, s := range existingPrincipal {
			if s == principal {
				foundPrincipal = true
				break
			}
		}
		if !foundPrincipal {
			permissionsToUpdate["Principal"].(map[string]interface{})["AWS"] = append(existingPrincipal, principal)
			updateRequired = true
		}
	}

	if updateRequired {
		b, err := json.MarshalIndent(policyJson, "", "   ")
		if err != nil {
			return fmt.Errorf("unable to marshal updated policy json, kms key %s: %w", kmsKeyArn, err)
		}
		if _, err := kmsSvc.PutKeyPolicy(ctx, &kms.PutKeyPolicyInput{
			KeyId:  ptr.To(kmsKeyArn),
			Policy: ptr.To(string(b)),
		}); err != nil {
			return fmt.Errorf("unable to update kms policy for key %s: policy %s: %w", kmsKeyArn, string(b), err)
		}
		log.Info("kms key permissions updated", "keyArn", kmsKeyArn, "newPermissions", string(b))
	}
	return nil
}

func convertToQuotedString(arr []string) []string {
	var quotedStrings []string
	for _, str := range arr {
		quotedStrings = append(quotedStrings, fmt.Sprintf("\"%s\"", str))
	}
	return quotedStrings
}

func RemoveKmsKeyPermission(ctx context.Context, cfg aws.Config, permissionName, kmsKeyArn string, removePrincipals []string) error {
	log := log.FromContext(ctx).WithName("RemoveKmsKeyPermission")
	kmsSvc := kms.NewFromConfig(cfg)

	keyPolicy, err := kmsSvc.GetKeyPolicy(ctx, &kms.GetKeyPolicyInput{
		KeyId: ptr.To(kmsKeyArn),
	})

	if err != nil {
		return fmt.Errorf("failed to get kms key policy, key arn %s: %w", kmsKeyArn, err)
	}
	policy := *keyPolicy.Policy

	var policyJson map[string]interface{}
	// unmarshal as any Json as statements data structure can vary and using a go object type can cause a loss on a re-marshall
	json.Unmarshal([]byte(policy), &policyJson)
	statements := policyJson["Statement"].([]interface{})

	foundPerms := false
	updateRequired := false
	permsStmtIndex := -1
	for idx, st := range statements {
		stMap := st.(map[string]interface{})
		if _, okst := stMap["Sid"]; okst {
			if stMap["Sid"].(string) == permissionName {
				foundPerms = true
				permsStmtIndex = idx
				break
			}
		}
	}

	if foundPerms {
		var permissionsToUpdate map[string]interface{}
		permissionsToUpdate = statements[permsStmtIndex].(map[string]interface{})
		removePermsStatement := false
		for _, principal := range removePrincipals {

			existingPrincipal, arrayOk := permissionsToUpdate["Principal"].(map[string]interface{})["AWS"].([]interface{})
			if !arrayOk {
				singlePrincipal, singleOk := permissionsToUpdate["Principal"].(map[string]interface{})["AWS"].(string)
				if !singleOk {
					return fmt.Errorf("invalid policy document found for kms key %s", kmsKeyArn)
				}
				existingPrincipal = []interface{}{singlePrincipal}
			}

			foundPrincipal := false
			foundIndex := -1
			for i, s := range existingPrincipal {
				if s == principal {
					foundPrincipal = true
					foundIndex = i
					break
				}
			}
			if foundPrincipal {
				updateRequired = true
				// remove statement block, if we had a single entry left for removal
				if len(existingPrincipal) <= 1 {
					permissionsToUpdate["Principal"].(map[string]interface{})["AWS"] = []interface{}{}
					removePermsStatement = true
					break
				} else {
					permissionsToUpdate["Principal"].(map[string]interface{})["AWS"] = append(existingPrincipal[:foundIndex], existingPrincipal[foundIndex+1:]...)
				}
			}
		}

		if removePermsStatement {
			//remove permission statement if all data plane perms have been removed (edge case as there will always be one data-plane/shared present)
			policyJson["Statement"] = append(statements[:permsStmtIndex], statements[permsStmtIndex+1:]...)
		}

		if updateRequired {
			b, err := json.MarshalIndent(policyJson, "", "   ")
			if err != nil {
				return fmt.Errorf("unable to marshal updated policy json, kms key %s: %w", kmsKeyArn, err)
			}
			if _, err := kmsSvc.PutKeyPolicy(ctx, &kms.PutKeyPolicyInput{
				KeyId:  ptr.To(kmsKeyArn),
				Policy: ptr.To(string(b)),
			}); err != nil {
				return fmt.Errorf("unable to update kms policy for key %s after removing principals: policy %s: %w", kmsKeyArn, string(b), err)
			}
			log.Info("kms key permissions updated after removing principals", "keyArn", kmsKeyArn, "newPermissions", string(b))
		}
	}
	return nil
}

// add or update a secret permissions for list of cross account principals
// Important !!! pass a unique permissionName to avoid updates to other kms key policy (e.g. add a tenant id in the permissionName)
// note this function updates both secret permissions as well as associated kms key arn permissions
// Also, note that it will fully update/overwrite the secret resource permissions for cross account access using the list of allowedPrincipals with read access
// The kms key associated with the secret is updated such that it retains any other permissions present in the key policy, i.e. kms key could be a shared key
func AddOrUpdateCrossAccountSecretReadPermissions(ctx context.Context, cfg aws.Config, secretName string, permissionName, allowedPrincipals []string) error {
	log := log.FromContext(ctx).WithName("AddOrUpdateCrossAccountSecretReadPermissions")
	kmsSvc := kms.NewFromConfig(cfg)

	secretSvc := secretsmanager.NewFromConfig(cfg)
	describeSecretOutput, err := secretSvc.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{
		SecretId: ptr.To(secretName),
	})
	if err != nil {
		return fmt.Errorf("failed to describe secret %s: %w", secretName, err)
	}
	kmsKey, err := kmsSvc.DescribeKey(ctx, &kms.DescribeKeyInput{
		KeyId: describeSecretOutput.KmsKeyId,
	})

	if err != nil {
		return fmt.Errorf("failed to retrieve kms key for the secret %s: %w", secretName, err)
	}
	kmsActions := []string{
		"kms:Decrypt",
		"kms:DescribeKey",
	}
	err = UpdateKmsKeyPermission(ctx, cfg, fmt.Sprintf("%sKms", permissionName), kmsActions, *kmsKey.KeyMetadata.Arn, allowedPrincipals)
	if err != nil {
		return err
	}
	quotedPrincipals := convertToQuotedString(allowedPrincipals)

	// secretsmanager resource policy, note these only apply to cross account access in the context of Secrets Manager secret
	secretsManagerResourcePolicy := fmt.Sprintf(`{
		"Version" : "2012-10-17",
		"Statement" : [ {
		  "Effect" : "Allow",
		  "Principal" : {
			"AWS" : [ %s ]
		  },
		  "Action" : "secretsmanager:GetSecretValue",
		  "Resource" : "*"
		} ]
	  }`, strings.Join(quotedPrincipals, ","))

	_, err = secretSvc.PutResourcePolicy(ctx, &secretsmanager.PutResourcePolicyInput{
		SecretId:          ptr.To(secretName),
		BlockPublicPolicy: ptr.To(true),
		ResourcePolicy:    ptr.To(secretsManagerResourcePolicy),
	})
	if err != nil {
		return fmt.Errorf("failed to update secret permissions %s: %s", secretName, err)
	}
	log.Info("updated secret for Dataplane", "secretName", secretName)
	return nil
}

// add ecr actions
/* ecrActions example for read
"ecr:BatchCheckLayerAvailability",
			  "ecr:BatchGetImage",
			  "ecr:DescribeImages",
			  "ecr:DescribeRepositories",
			  "ecr:GetDownloadUrlForLayer
*/

func UpdateCrossAccountEcrReadPermissions(ctx context.Context, cfg aws.Config, repositoryName, permissionName string, ecrActions []string, kmsKeyArn string, allowedPrincipals []string) error {
	log := log.FromContext(ctx).WithName("UpdateCrossAccountEcrReadPermissions")

	ecrSvc := ecr.NewFromConfig(cfg)
	repos, err := ecrSvc.DescribeRepositories(ctx, &ecr.DescribeRepositoriesInput{
		RepositoryNames: []string{repositoryName},
	})

	if err != nil || len(repos.Repositories) == 0 {
		return fmt.Errorf("unable to describe repository %s", repositoryName)
	}

	ecrPolicy, err := ecrSvc.GetRepositoryPolicy(ctx, &ecr.GetRepositoryPolicyInput{
		RepositoryName: ptr.To(repositoryName),
	})

	if err != nil {
		return fmt.Errorf("failed to get kms key policy, key arn %s: %w", kmsKeyArn, err)
	}
	policy := *ecrPolicy.PolicyText

	var policyJson map[string]interface{}
	// unmarshal as any Json as statements data structure can vary and using a go object type can cause a loss on a re-marshall
	json.Unmarshal([]byte(policy), &policyJson)
	statements := policyJson["Statement"].([]interface{})

	foundPerms := false
	updateRequired := false
	permsStmtIndex := -1
	for idx, st := range statements {
		stMap := st.(map[string]interface{})
		if _, okst := stMap["Sid"]; okst {
			if stMap["Sid"].(string) == permissionName {
				foundPerms = true
				permsStmtIndex = idx
				break
			}
		}
	}

	var permissionsToUpdate map[string]interface{}

	if !foundPerms {
		quotedPrincipals := convertToQuotedString(allowedPrincipals)
		quotedEcrActions := convertToQuotedString(ecrActions)
		ecrStatement := fmt.Sprintf(`{
				"Sid": "AllowPull",
				"Effect": "Allow",
				"Principal": {"AWS": [ %s ],
				"Action": [ %s ]
		}`, strings.Join(quotedPrincipals, ","), strings.Join(quotedEcrActions, ","))

		var addPermissions map[string]interface{}
		json.Unmarshal([]byte(ecrStatement), &addPermissions)

		policyJson["Statement"] = append(statements, addPermissions)
		permissionsToUpdate = addPermissions
		updateRequired = true
	} else {
		permissionsToUpdate = statements[permsStmtIndex].(map[string]interface{})
		permissionsToUpdate["Action"] = ecrActions
	}

	for _, principal := range allowedPrincipals {
		existingPrincipal, arrayOk := permissionsToUpdate["Principal"].(map[string]interface{})["AWS"].([]interface{})
		if !arrayOk {
			// aws changes a single principal to a non array element
			singlePrincipal, singleOk := permissionsToUpdate["Principal"].(map[string]interface{})["AWS"].(string)
			if !singleOk {
				return fmt.Errorf("invalid policy document found for ecr %s", repositoryName)
			}
			existingPrincipal = []interface{}{singlePrincipal}
		}
		foundPrincipal := false
		for _, s := range existingPrincipal {
			if s == principal {
				foundPrincipal = true
				break
			}
		}
		if !foundPrincipal {
			permissionsToUpdate["Principal"].(map[string]interface{})["AWS"] = append(existingPrincipal, principal)
			updateRequired = true
		}
	}

	if updateRequired {
		b, err := json.MarshalIndent(policyJson, "", "   ")
		if err != nil {
			return fmt.Errorf("unable to marshal updated policy json, kms key %s: %w", kmsKeyArn, err)
		}
		if _, err := ecrSvc.SetRepositoryPolicy(ctx, &ecr.SetRepositoryPolicyInput{
			RepositoryName: ptr.To(repositoryName),
			PolicyText:     ptr.To(string(b)),
		}); err != nil {
			return fmt.Errorf("unable to update kms policy for key %s: policy %s: %w", kmsKeyArn, string(b), err)
		}
		log.Info("ecr policy updated", "repository", repositoryName, "newPermissions", string(b))
	}

	return nil
}

// Update EMPTYOIDC IRSA role cluster issuer ids:
// When we bootstrap a dataplane within account/iamroles we do not have EKS OIDC available (as eks is not yet provisioned).
// as a workaround if eks is not available when applying account role (i.e. first time) the terraform set assume role policy document to EMPTYOIDC.
// The account layer can be re-applied after applying eks layer to udpate IRSA OIDC, but to avoid re-apply step, we can also scan for IRSA specific roles to see if EMPTYOIDC is present in policy and update these to the EKS OIDC
// the update is done only if EMPTYOIDC is detected and needs a replacement
func UpdateEmptyOIDCRoleTrustPolicy(ctx context.Context, cfg aws.Config, issuerID, roleName string) error {

	iamclient := iam.NewFromConfig(cfg)
	role, err := iamclient.GetRole(ctx, &iam.GetRoleInput{RoleName: ptr.To(roleName)})
	if err != nil {
		return fmt.Errorf("unable to describe role %s", roleName)
	}
	assumeRolePolicy := *role.Role.AssumeRolePolicyDocument
	if strings.HasPrefix(assumeRolePolicy, "%7B") {
		//Ref: https://github.com/aws/aws-sdk-go-v2/issues/227
		assumeRolePolicy, err = url.QueryUnescape(assumeRolePolicy)
		if err != nil {
			return fmt.Errorf("unable to url decode assume role policy document, role %s: %w", roleName, err)
		}
	}

	unInitializedMagicOIDC := "EMPTYOIDC"
	newPolicy := strings.ReplaceAll(assumeRolePolicy, unInitializedMagicOIDC, issuerID)
	if newPolicy != assumeRolePolicy {
		if _, err := iamclient.UpdateAssumeRolePolicy(ctx, &iam.UpdateAssumeRolePolicyInput{
			RoleName:       aws.String(roleName),
			PolicyDocument: aws.String(strings.TrimSpace(newPolicy)),
		}); err != nil {
			return fmt.Errorf("unable to update assume role policy document, role %s: %w", roleName, err)
		}
	}
	return nil
}
