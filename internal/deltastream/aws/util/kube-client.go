// Copyright (c) DeltaStream, Inc.
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"text/template"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	helmv2 "github.com/fluxcd/helm-controller/api/v2beta2"
	imageautov1 "github.com/fluxcd/image-automation-controller/api/v1beta1"
	imagereflectv1 "github.com/fluxcd/image-reflector-controller/api/v1beta2"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	notificationv1 "github.com/fluxcd/notification-controller/api/v1"
	notificationv1b3 "github.com/fluxcd/notification-controller/api/v1beta3"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	sourcev1b2 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/jellydator/ttlcache/v3"
	"github.com/sethvargo/go-retry"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/karpenter/pkg/apis"
	karpenterv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/yaml"

	awsconfig "github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws/config"
)

const eksConfigTemplate = `apiVersion: v1
clusters:
- cluster:
    server: {{ .Endpoint }}
    certificate-authority-data: {{ .CAData }}
  name: kubernetes
contexts:
- context:
    cluster: kubernetes
    user: aws
  name: aws
current-context: aws
kind: Config
preferences: {}
users:
- name: aws
  user:
    token: {{ .Token }}
`

type customPresignClient struct {
	client      sts.HTTPPresignerV4
	clusterName string
}

const cacheTimeout = time.Second * 500 // must be less than X-Amz-Expires

func (p *customPresignClient) PresignHTTP(ctx context.Context, credentials aws.Credentials, req *http.Request, payloadHash string, service string, region string, signingTime time.Time, optFns ...func(*v4.SignerOptions)) (url string, signedHeader http.Header, err error) {
	req.Header.Add("x-k8s-aws-id", p.clusterName)
	req.Header.Add("X-Amz-Expires", "600")
	return p.client.PresignHTTP(ctx, credentials, req, payloadHash, service, region, signingTime, optFns...)
}

func getKubernetesAuthToken(ctx context.Context, cfg aws.Config, k8sClusterName string) (token string, err error) {
	stsclient := sts.NewPresignClient(sts.NewFromConfig(cfg))

	presignedReq, err := stsclient.PresignGetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}, func(po *sts.PresignOptions) {
		po.Presigner = &customPresignClient{
			client:      po.Presigner,
			clusterName: k8sClusterName,
		}
	})
	if err != nil {
		return "", fmt.Errorf("unable to initiate presigned caller identity: %w", err)
	}

	presignedURL, err := url.Parse(presignedReq.URL)
	if err != nil {
		return "", fmt.Errorf("unable to parse presigned URL: %w", err)
	}

	return "k8s-aws-v1." + base64.RawURLEncoding.EncodeToString([]byte(presignedURL.String())), nil
}

func GetKubeClusterName(ctx context.Context, dp awsconfig.AWSDataplane) (name string, err error) {
	clusterConfigurationData, diags := dp.ClusterConfigurationData(ctx)
	if diags.HasError() {
		return "", fmt.Errorf("failed to get cluster configuration data: %v", diags.Errors())
	}

	return fmt.Sprintf("ds-%s-%s-%s-%d", clusterConfigurationData.InfraId.ValueString(), clusterConfigurationData.Stack.ValueString(), clusterConfigurationData.EksResourceId.ValueString(), ptr.Deref(clusterConfigurationData.ClusterIndex.ValueInt64Pointer(), 0)), nil
}

func DescribeKubeCluster(ctx context.Context, dp awsconfig.AWSDataplane, cfg aws.Config) (cluster *types.Cluster, err error) {
	clusterName, err := GetKubeClusterName(ctx, dp)
	if err != nil {
		return nil, err
	}

	eksClient := eks.NewFromConfig(cfg)
	ekcDescOut, err := eksClient.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String(clusterName)})
	if err != nil {
		return nil, fmt.Errorf("failed to describe EKS cluster: %w", err)
	}

	cluster = ekcDescOut.Cluster
	if cluster == nil || cluster.Endpoint == nil || cluster.CertificateAuthority == nil || cluster.CertificateAuthority.Data == nil {
		return nil, fmt.Errorf("failed to get EKS cluster: cluster data is nil")
	}
	return cluster, nil
}

func GetKubeConfig(ctx context.Context, dp awsconfig.AWSDataplane, cfg aws.Config) (kubeConfig []byte, err error) {
	cluster, err := DescribeKubeCluster(ctx, dp, cfg)
	if err != nil {
		return nil, err
	}

	t, err := template.New("eksConfig").Parse(eksConfigTemplate)
	if err != nil {
		return nil, fmt.Errorf("failed to parse kubeconfig template: %w", err)
	}

	token, err := getKubernetesAuthToken(ctx, cfg, *cluster.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to get k8s token: %w", err)
	}

	kubeConfigBuf := bytes.NewBuffer(nil)
	err = t.Execute(kubeConfigBuf, map[string]string{
		"Endpoint": *cluster.Endpoint,
		"CAData":   *cluster.CertificateAuthority.Data,
		"Token":    token,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to execute kubeconfig template: %w", err)
	}
	return kubeConfigBuf.Bytes(), nil
}

var kubeClientCache = ttlcache.New[string, *RetryableClient]()

func GetKubeClient(ctx context.Context, cfg aws.Config, dp awsconfig.AWSDataplane) (rClient *RetryableClient, err error) {
	kubeClientCache.DeleteExpired()
	if v := kubeClientCache.Get("kubeClient"); v != nil {
		tflog.Debug(ctx, "reusing kube client")
		return v.Value(), nil
	}
	tflog.Debug(ctx, "creating new kube client")

	kubeconfig, err := GetKubeConfig(ctx, dp, cfg)
	if err != nil {
		return nil, err
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kube client config: %w", err)
	}

	scheme := runtime.NewScheme()
	if err = clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add client-go scheme: %w", err)
	}

	apiextensionsv1.AddToScheme(scheme)
	_ = sourcev1b2.AddToScheme(scheme)
	_ = sourcev1.AddToScheme(scheme)
	_ = kustomizev1.AddToScheme(scheme)
	_ = helmv2.AddToScheme(scheme)
	_ = notificationv1.AddToScheme(scheme)
	_ = notificationv1b3.AddToScheme(scheme)
	_ = imagereflectv1.AddToScheme(scheme)
	_ = imageautov1.AddToScheme(scheme)

	//karpenter
	scheme.AddKnownTypes(schema.GroupVersion{Group: apis.Group, Version: "v1"}, &karpenterv1.NodeClaimList{}, &karpenterv1.NodeClaim{})

	kubeClient, err := client.New(restConfig, client.Options{
		Scheme: scheme,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create kube client: %w", err)
	}
	rClient = &RetryableClient{Client: kubeClient}

	kubeClientCache.Set("kubeClient", rClient, cacheTimeout)

	return
}

func GetKubeClientSets(ctx context.Context, cfg aws.Config, dp awsconfig.AWSDataplane) (clientSet *kubernetes.Clientset, err error) {
	tflog.Debug(ctx, "creating new kube client")

	kubeconfig, err := GetKubeConfig(ctx, dp, cfg)
	if err != nil {
		return nil, err
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kube client config: %w", err)
	}

	clientSet, err = kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create k8s clientset: %v", err)
	}
	return
}

func ApplyManifests(ctx context.Context, kubeClient *RetryableClient, manifestYamlsCombined string) (d diag.Diagnostics) {
	manifestYamls := strings.Split(manifestYamlsCombined, "\n---\n")
	for _, manifestYaml := range manifestYamls {
		u := &unstructured.Unstructured{}

		manifestLog := fmt.Sprintf("Unmarshalling manifest: %s", strings.ReplaceAll(manifestYaml, "\n", "\\n"))
		tflog.Debug(ctx, manifestLog)
		if err := yaml.Unmarshal([]byte(manifestYaml), u); err != nil {
			d.AddError("Failed to unmarshal manifest", err.Error())
			return
		}

		tflog.Info(ctx, "Applying object", map[string]any{
			"kind": u.GetKind(),
			"name": u.GetName(),
		})

		if err := retry.Do(ctx, retry.WithMaxRetries(5, retry.NewExponential(time.Second)), func(ctx context.Context) error {
			ug := u.DeepCopy()
			if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(ug), ug); err != nil {
				if k8serrors.IsNotFound(err) {
					if err := kubeClient.Create(ctx, u); err != nil {
						return retry.RetryableError(err)
					}
					return nil
				}
				return retry.RetryableError(err)
			}

			u.SetResourceVersion(ug.GetResourceVersion())
			if err := kubeClient.Update(ctx, u); err != nil {
				errorLog := fmt.Sprintf("Error updating %s %s: %v", u.GetKind(), u.GetName(), err)
				tflog.Debug(ctx, errorLog)
				return retry.RetryableError(err)
			}
			return nil
		}); err != nil {
			d.AddError("Failed to create manifest", err.Error())
			return
		}
	}
	return
}

func RenderAndApplyTemplate(ctx context.Context, kubeClient *RetryableClient, name string, templateData []byte, data map[string]string) (d diag.Diagnostics) {
	tflog.Debug(ctx, "rendering manifest template "+name)
	t, err := template.New(name).Parse(string(templateData))
	if err != nil {
		d.AddError("error parsing manifest template "+name, err.Error())
		return
	}

	b := bytes.NewBuffer(nil)
	if err := t.Execute(b, data); err != nil {
		d.AddError("error render manifest template "+name, err.Error())
		return
	}

	return ApplyManifests(ctx, kubeClient, b.String())
}

type RetryableClient struct {
	Client client.Client
}

var retrylimits = retry.WithMaxRetries(20, retry.NewConstant(time.Second*20))

func (r *RetryableClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return retry.Do(ctx, retrylimits, func(ctx context.Context) error {
		if err := r.Client.Create(ctx, obj, opts...); err != nil {
			if k8serrors.IsAlreadyExists(err) {
				return err
			}
			tflog.Debug(ctx, "create error "+err.Error())
			return retry.RetryableError(err)
		}
		return nil
	})
}

func (r *RetryableClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	return retry.Do(ctx, retrylimits, func(ctx context.Context) error {
		if err := r.Client.Delete(ctx, obj, opts...); err != nil {
			if k8serrors.IsNotFound(err) {
				return err
			}
			tflog.Debug(ctx, "delete error "+err.Error())
			return retry.RetryableError(err)
		}
		return nil
	})
}

func (r *RetryableClient) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	return retry.Do(ctx, retrylimits, func(ctx context.Context) error {
		if err := r.Client.DeleteAllOf(ctx, obj, opts...); err != nil {
			tflog.Debug(ctx, "delete all error "+err.Error())
			return retry.RetryableError(err)
		}
		return nil
	})
}

func (r *RetryableClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	return retry.Do(ctx, retrylimits, func(ctx context.Context) error {
		if err := r.Client.Patch(ctx, obj, patch, opts...); err != nil {
			if k8serrors.IsNotFound(err) {
				return err
			}
			tflog.Debug(ctx, "patch error "+err.Error())
			return retry.RetryableError(err)
		}
		return nil
	})
}

func (r *RetryableClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return retry.Do(ctx, retrylimits, func(ctx context.Context) error {
		if err := r.Client.Update(ctx, obj, opts...); err != nil {
			if k8serrors.IsNotFound(err) {
				return err
			}
			tflog.Debug(ctx, "update error "+err.Error())
			return retry.RetryableError(err)
		}
		return nil
	})
}

func (r *RetryableClient) Get(ctx context.Context, key k8stypes.NamespacedName, obj client.Object, opts ...client.GetOption) error {
	return retry.Do(ctx, retrylimits, func(ctx context.Context) error {
		if err := r.Client.Get(ctx, key, obj, opts...); err != nil {
			if k8serrors.IsNotFound(err) {
				return err
			}
			tflog.Debug(ctx, "get error for "+key.Name+" ns: "+key.Namespace+" err: "+err.Error())
			return retry.RetryableError(err)
		}
		return nil
	})
}

func (r *RetryableClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return retry.Do(ctx, retrylimits, func(ctx context.Context) error {
		if err := r.Client.List(ctx, list, opts...); err != nil {
			tflog.Debug(ctx, "list error "+err.Error())
			return retry.RetryableError(err)
		}
		return nil
	})
}

var _ client.Reader = &RetryableClient{}
var _ client.Writer = &RetryableClient{}
