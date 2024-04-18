package util

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

func LogError(ctx context.Context, d diag.Diagnostics, summary string, err error) diag.Diagnostics {
	tflog.Info(ctx, err.Error())
	d.AddError(summary, err.Error())
	return d
}
