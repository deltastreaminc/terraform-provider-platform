// Copyright (c) DeltaStream, Inc.
// SPDX-License-Identifier: Apache-2.0

package dataplanemodel

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
)

type VPCSettings struct {
	AccountID       types.String `tfsdk:"account_id"`
	Region          types.String `tfsdk:"region"`
	DrRegion        types.String `tfsdk:"dr_region"`
	CIDR            types.String `tfsdk:"cidr"`
	AuthorizedUsers types.List   `tfsdk:"authorized_users"`
	AuthorizedRoles types.List   `tfsdk:"authorized_roles"`
}

type State struct {
	GenerationID   types.Int64  `tfsdk:"generation_id"`
	BootstrapKmsId types.String `tfsdk:"bootstrap_kms_id"`
}

// DataplaneResourceModel describes the resource data model.
type Resource struct {
	VPC            types.Object `tfsdk:"vpc"`
	Name           types.String `tfsdk:"name"`
	Tags           types.Map    `tfsdk:"tags"`
	RequireRefresh types.Bool   `tfsdk:"require_refresh"`
	State          types.Object `tfsdk:"state"`
}

func (r *Resource) GetVPC(ctx context.Context) (VPCSettings, diag.Diagnostics) {
	var vpc VPCSettings
	diag := r.VPC.As(ctx, &vpc, basetypes.ObjectAsOptions{})
	return vpc, diag
}

var Schema = schema.Schema{
	MarkdownDescription: "Dataplane resource",

	Attributes: map[string]schema.Attribute{
		"vpc": schema.SingleNestedAttribute{
			Description: "VPC settings",
			Attributes: map[string]schema.Attribute{
				"cidr": schema.StringAttribute{
					Description: "CIDR block for the VPC",
					Required:    true,
				},
				"account_id": schema.StringAttribute{
					Description: "Account ID for the VPC",
					Required:    true,
				},
				"region": schema.StringAttribute{
					Description: "Region for the VPC",
					Required:    true,
				},
				"dr_region": schema.StringAttribute{
					Description: "DR region for the VPC",
					Required:    true,
				},
				"authorized_users": schema.ListAttribute{
					Description: "Authorized users for the VPC",
					ElementType: basetypes.StringType{},
					Required:    true,
				},
				"authorized_roles": schema.ListAttribute{
					Description: "Authorized roles for the VPC",
					ElementType: basetypes.StringType{},
					Required:    true,
				},
			},
			Required: true,
		},
		"name": schema.StringAttribute{
			Description: "Name of the dataplane",
			Required:    true,
		},
		"tags": schema.MapAttribute{
			Description: "Tags for the dataplane",
			ElementType: types.StringType,
			Optional:    true,
		},
		"require_refresh": schema.BoolAttribute{
			Description: "Trigger a refresh of AWS resources managed by this provider",
			Optional:    true,
		},
		"state": schema.SingleNestedAttribute{
			Description: "Recorded state of the dataplane",
			Attributes: map[string]schema.Attribute{
				"updated_at": schema.Int64Attribute{
					Description: "Timestamp of the last update",
					Computed:    true,
				},
				"bootstrap_kms_id": schema.StringAttribute{
					Description: "ID of the bootstrap KMS key",
					Optional:    true,
					Computed:    true,
				},
			},
			Optional: true,
			Computed: true,
		},
	},
}
