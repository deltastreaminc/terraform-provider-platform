// Copyright (c) DeltaStream, Inc.
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/alitto/pond"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/docker"
	"github.com/containers/image/v5/signature"
	"github.com/containers/image/v5/types"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/sethvargo/go-retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	awsconfig "github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/config"
)

func copyImages(ctx context.Context, cfg aws.Config, dp awsconfig.AWSDataplane) (execEngineVersion string, d diag.Diagnostics) {
	clusterConfig, diags := dp.ClusterConfigurationData(ctx)
	d.Append(diags...)
	if d.HasError() {
		return
	}

	bucketName := "prod-ds-packages-maven"
	if clusterConfig.Stack.ValueString() != "prod" {
		bucketName = "deltastream-packages-maven"
	}

	bucketCfg := cfg.Copy()
	bucketCfg.Region = "us-east-2"
	s3client := s3.NewFromConfig(bucketCfg)
	imageListPath := fmt.Sprintf("deltastreamv2-release-images/image-list-%s.yaml", clusterConfig.ProductVersion.ValueString())
	tflog.Debug(ctx, "downloading image list", map[string]any{
		"bucket":          bucketName,
		"image list path": imageListPath,
	})
	getObjectOut, err := s3client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(imageListPath),
	})
	if err != nil {
		d.AddError("error getting image list", err.Error())
		return
	}
	defer getObjectOut.Body.Close()

	imageList := struct {
		Images            []string `json:"images"`
		ExecEngineVersion string   `json:"execEngineVersion"`
	}{}

	b, err := io.ReadAll(getObjectOut.Body)
	if err != nil {
		d.AddError("error reading image list", err.Error())
		return
	}
	if err := yaml.Unmarshal(b, &imageList); err != nil {
		d.AddError("error unmarshalling image list", err.Error())
		return
	}

	// Create an Amazon ECR service client
	client := ecr.NewFromConfig(cfg)

	authTokenOut, err := client.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		d.AddError("error getting authorization token", err.Error())
		return
	}

	tokenBytes, err := base64.StdEncoding.DecodeString(*authTokenOut.AuthorizationData[0].AuthorizationToken)
	if err != nil {
		d.AddError("error decoding authorization token", err.Error())
		return
	}
	imageCredContext := &types.SystemContext{
		DockerAuthConfig: &types.DockerAuthConfig{
			Username: "AWS",
			Password: strings.TrimPrefix(string(tokenBytes), "AWS:"),
		},
	}

	// copy if source and destination Ecrs are different, e.g. we may have combined plane as multi-tenant plane within the same account where prod Ecr is maintained
	if clusterConfig.DsAccountId.ValueString() != clusterConfig.AccountId.ValueString() {
		pool := pond.New(1, 10)
		defer pool.StopAndWait()
		group := pool.Group()
	
		// dedup the image list
		imageMap := make(map[string]bool)
		for _, image := range imageList.Images {
			imageMap[image] = true
		}
	
		for image := range imageMap {
			sourceImage := fmt.Sprintf("//%s.dkr.ecr.%s.amazonaws.com/%s", clusterConfig.DsAccountId.ValueString(), cfg.Region, image)
			destImage := fmt.Sprintf("//%s.dkr.ecr.%s.amazonaws.com/%s", clusterConfig.AccountId.ValueString(), cfg.Region, image)
	
			destRepository := strings.Split(image, ":")[0]
			// check if image exist in destination account, if not then create it
			_, err := client.DescribeRepositories(ctx, &ecr.DescribeRepositoriesInput{
				RepositoryNames: []string{destRepository},
			})
			if err != nil {
				var notFound *ecrtypes.RepositoryNotFoundException
				if !errors.As(err, &notFound) {
					d.AddError(fmt.Sprintf("error copying image, unable to describe repository %s in destination registry", destRepository), err.Error())
					return
				} else {
					_, err = client.CreateRepository(ctx, &ecr.CreateRepositoryInput{
						RepositoryName:     ptr.To(destRepository),
						ImageTagMutability: ecrtypes.ImageTagMutabilityMutable,
						Tags: []ecrtypes.Tag{
							{Key: ptr.To("managed-by"),
								Value: ptr.To("deltastream.io"),
							}},
					})
					if err != nil {
						d.AddError(fmt.Sprintf("error copying image, unable to create repository %s in destination registry", destRepository), err.Error())
						return
					}
				}
			}
	
			group.Submit(func() {
				err = copyImage(ctx, imageCredContext, sourceImage, destImage)
				if err != nil {
					d.AddError("error copying image", err.Error())
					return
				}
			})
		}
	
		group.Wait()
	}

	execEngineUri := fmt.Sprintf("deltastreamv2-release-images/exec-engine/%s/release/io/deltastream/execution-engine/%s/execution-engine-%s.jar", clusterConfig.ProductVersion.ValueString(), imageList.ExecEngineVersion, imageList.ExecEngineVersion)
	destExecEngineUri := fmt.Sprintf("release/io/deltastream/execution-engine/%s/execution-engine-%s.jar", imageList.ExecEngineVersion, imageList.ExecEngineVersion)
	// Copy the execution engine jar
	tflog.Debug(ctx, "downloading execution engine jar "+bucketName+" "+execEngineUri)
	getObjectOut, err = s3client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(execEngineUri),
	})
	if err != nil {
		d.AddError("error downloading execution engine jar", err.Error())
		return
	}
	defer getObjectOut.Body.Close()
	b, err = io.ReadAll(getObjectOut.Body)
	if err != nil {
		d.AddError("error reading execution engine jar", err.Error())
		return
	}

	tflog.Debug(ctx, "uploading execution engine jar", map[string]any{
		"bucket": clusterConfig.ProductArtifactsBucket.ValueString(),
		"uri":    destExecEngineUri,
		"size":   len(b),
	})
	uploadS3Client := s3.NewFromConfig(cfg)
	// Upload the execution engine jar to the new bucket
	_, err = uploadS3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(clusterConfig.ProductArtifactsBucket.ValueString()),
		Key:    aws.String(destExecEngineUri),
		Body:   bytes.NewBuffer(b),
	})
	if err != nil {
		d.AddError("error uploading execution engine jar", err.Error())
		return
	}

	execEngineVersion = imageList.ExecEngineVersion

	return
}

type imgBlob struct {
	copiedBytes float64
	totalBytes  float64
}

func copyImage(ctx context.Context, credContext *types.SystemContext, sourceImage, destImage string) (err error) {
	tflog.Debug(ctx, "copying image", map[string]any{
		"source": sourceImage,
		"dest":   destImage,
	})

	srcRef, err := docker.ParseReference(sourceImage)
	if err != nil {
		return fmt.Errorf("error parsing source image: %w", err)
	}

	destRef, err := docker.ParseReference(destImage)
	if err != nil {
		return fmt.Errorf("error parsing destination image: %w", err)
	}

	policy := &signature.Policy{Default: []signature.PolicyRequirement{signature.NewPRInsecureAcceptAnything()}}
	policyContext, err := signature.NewPolicyContext(policy)
	if err != nil {
		return fmt.Errorf("error creating new policy context: %w", err)
	}

	b := bytes.NewBuffer(nil)
	reportCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	progressChan := make(chan types.ProgressProperties)

	go func() {
		blobs := map[string]imgBlob{}
		for {
			select {
			case <-reportCtx.Done():
				return
			case p := <-progressChan:
				switch p.Event {
				case types.ProgressEventDone:
					tflog.Info(ctx, fmt.Sprintf("%s: 100.0\n", destImage))
				}

				blobs[p.Artifact.Digest.String()] = imgBlob{
					copiedBytes: float64(p.Offset),
					totalBytes:  float64(p.Artifact.Size),
				}

				copiedBytes := float64(0)
				totalBytes := float64(0)
				for _, a := range blobs {
					copiedBytes += a.copiedBytes
					totalBytes += a.totalBytes
				}
				if totalBytes == 0 {
					continue
				}
				pct := 100.0 * (float32(copiedBytes) / float32(totalBytes))
				tflog.Info(ctx, fmt.Sprintf("%s: %f\n", destImage, pct))
			}
		}
	}()

	err = retry.Do(ctx, retry.WithMaxRetries(20, retry.NewExponential(time.Second*5)), func(ctx context.Context) error {
		_, err := copy.Image(ctx, policyContext, destRef, srcRef, &copy.Options{
			SourceCtx:          credContext,
			DestinationCtx:     credContext,
			ReportWriter:       b,
			Progress:           progressChan,
			ProgressInterval:   time.Second * 10,
			PreserveDigests:    true,
			ImageListSelection: copy.CopyAllImages,
		})
		if err != nil {
			tflog.Error(ctx, err.Error())
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("error copying image: %w\n%s", err, b.String())
	}

	return
}
