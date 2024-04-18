// Copyright (c) DeltaStream, Inc.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/function"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"

	"github.com/deltastreaminc/terraform-provider-platform/internal/config"
	"github.com/deltastreaminc/terraform-provider-platform/internal/deltastream/aws"
)

// Ensure ScaffoldingProvider satisfies various provider interfaces.
var _ provider.Provider = &DeltaStreamPlatformProvider{}
var _ provider.ProviderWithFunctions = &DeltaStreamPlatformProvider{}

// DeltaStreamPlatformProvider defines the provider implementation.
type DeltaStreamPlatformProvider struct {
	// version is the provider version. set by goreleaser.
	version string
}

// DeltaStreamPlatformProviderModel describes the provider data model.
type DeltaStreamPlatformProviderModel struct {
}

func (p *DeltaStreamPlatformProvider) Metadata(ctx context.Context, req provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "platform"
	resp.Version = p.version
}

func (p *DeltaStreamPlatformProvider) Schema(ctx context.Context, req provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "DeltaStream Platform provider",

		Attributes: map[string]schema.Attribute{},
	}
}
func (p *DeltaStreamPlatformProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var data DeltaStreamPlatformProviderModel

	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	resp.ResourceData = &config.PlatformResourceData{Version: p.version}
}

func (p *DeltaStreamPlatformProvider) Resources(ctx context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		aws.NewAWSDataplaneResource,
	}
}

func (p *DeltaStreamPlatformProvider) DataSources(ctx context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{}
}

func (p *DeltaStreamPlatformProvider) Functions(ctx context.Context) []func() function.Function {
	return []func() function.Function{}
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &DeltaStreamPlatformProvider{
			version: version,
		}
	}
}
