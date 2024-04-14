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
)

// Ensure ScaffoldingProvider satisfies various provider interfaces.
var _ provider.Provider = &DeltaStreamDataplaneProvider{}
var _ provider.ProviderWithFunctions = &DeltaStreamDataplaneProvider{}

// DeltaStreamDataplaneProvider defines the provider implementation.
type DeltaStreamDataplaneProvider struct {
	// version is the provider version. set by goreleaser.
	version string
}

// DeltaStreamDataplaneProviderModel describes the provider data model.
type DeltaStreamDataplaneProviderModel struct {
}

func (p *DeltaStreamDataplaneProvider) Metadata(ctx context.Context, req provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "deltastream-dataplane"
	resp.Version = p.version
}

func (p *DeltaStreamDataplaneProvider) Schema(ctx context.Context, req provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Attributes: map[string]schema.Attribute{},
	}
}
func (p *DeltaStreamDataplaneProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var data DeltaStreamDataplaneProviderModel

	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	resp.ResourceData = nil
}

func (p *DeltaStreamDataplaneProvider) Resources(ctx context.Context) []func() resource.Resource {
	return []func() resource.Resource{}
}

func (p *DeltaStreamDataplaneProvider) DataSources(ctx context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{}
}

func (p *DeltaStreamDataplaneProvider) Functions(ctx context.Context) []func() function.Function {
	return []func() function.Function{}
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &DeltaStreamDataplaneProvider{
			version: version,
		}
	}
}
