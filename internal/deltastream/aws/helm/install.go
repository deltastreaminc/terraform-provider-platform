// Copyright (c) DeltaStream, Inc.
// SPDX-License-Identifier: Apache-2.0

package helm

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"sigs.k8s.io/yaml"
)

func InstallRelease(ctx context.Context, kubeconfig []byte, namespace string, releaseName string, chartTarball io.Reader, values []byte, installOnly bool) error {
	clientGetter := NewRESTClientGetter(namespace, kubeconfig)

	actionConfig := &action.Configuration{}
	if err := actionConfig.Init(clientGetter, namespace, "secret", func(format string, v ...interface{}) {
		fmt.Printf(format, v)
	}); err != nil {
		return fmt.Errorf("unable to initialize helm: %w", err)
	}

	tflog.Debug(ctx, "looking up release", map[string]any{"release": releaseName, "namespace": namespace})
	getAction := action.NewGet(actionConfig)
	release, err := getAction.Run(releaseName)
	if err != nil {
		if !strings.Contains(err.Error(), "not found") {
			return fmt.Errorf("unable to get release: %w", err)
		}
		release = nil
		tflog.Debug(ctx, "release not found", map[string]any{"release": releaseName, "namespace": namespace})
	}

	chart, err := loader.LoadArchive(chartTarball)
	if err != nil {
		return fmt.Errorf("unable to load chart: %w", err)
	}

	h := sha256.New()
	h.Write(values)
	valueHash := base64.StdEncoding.EncodeToString(h.Sum(nil))
	valuesMap := map[string]interface{}{}
	if err = yaml.Unmarshal(values, &valuesMap); err != nil {
		return fmt.Errorf("unable to load values: %w", err)
	}

	if release != nil && release.Info != nil {
		tflog.Debug(ctx, "release found", map[string]any{"release": releaseName, "namespace": namespace, "installed value hash": release.Info.Description, "expected value hash": valueHash})
		if installOnly {
			return nil
		}

		if release.Info.Description != valueHash {
			upgradeAction := action.NewUpgrade(actionConfig)
			upgradeAction.Wait = true
			upgradeAction.Namespace = namespace
			upgradeAction.Description = valueHash
			upgradeAction.SkipCRDs = false
			upgradeAction.Atomic = true
			upgradeAction.Recreate = true
			upgradeAction.EnableDNS = false

			tflog.Debug(ctx, "upgrading release", map[string]any{"release": releaseName, "namespace": namespace})
			if _, err = upgradeAction.RunWithContext(ctx, releaseName, chart, valuesMap); err != nil {
				return fmt.Errorf("unable to upgrade release: %w", err)
			}
			return nil
		} else {
			tflog.Debug(ctx, "release already installed", map[string]any{"release": releaseName, "namespace": namespace})
			return nil
		}
	}

	installAction := action.NewInstall(actionConfig)
	installAction.ReleaseName = releaseName
	installAction.Wait = true
	installAction.Namespace = namespace
	installAction.IncludeCRDs = true
	installAction.Replace = true
	installAction.Description = valueHash
	installAction.EnableDNS = false
	installAction.Timeout = time.Minute * 10

	tflog.Debug(ctx, "installing release", map[string]any{"release": releaseName, "namespace": namespace})
	_, err = installAction.RunWithContext(ctx, chart, valuesMap)
	if err != nil {
		return fmt.Errorf("unable to install release: %w", err)
	}
	return nil
}
