/*
Copyright 2022 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package syncer

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/pkg/version"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/kcp-dev/logicalcluster/v3"

	edgev1alpha1 "github.com/kcp-dev/edge-mc/pkg/apis/edge/v1alpha1"
	syncerclientset "github.com/kcp-dev/edge-mc/pkg/client/clientset/versioned"
	syncerinformers "github.com/kcp-dev/edge-mc/pkg/client/informers/externalversions"
	edgev1alpha1listers "github.com/kcp-dev/edge-mc/pkg/client/listers/edge/v1alpha1"
	"github.com/kcp-dev/edge-mc/pkg/syncer/controller"
	"github.com/kcp-dev/edge-mc/pkg/syncer/syncers"
)

type SyncerConfig struct {
	UpstreamConfig   *rest.Config
	DownstreamConfig *rest.Config
	SyncTargetPath   logicalcluster.Path
	SyncTargetName   string
	SyncTargetUID    string
	Interval         time.Duration
}

const (
	resyncPeriod    = 10 * time.Hour
	defaultInterval = time.Second * 15
	minimumInterval = time.Second * 1
)

func RunSyncer(ctx context.Context, cfg *SyncerConfig, numSyncerThreads int) error {
	logger := klog.FromContext(ctx)
	logger = logger.WithValues("syncTargetName", cfg.SyncTargetName)
	logger.V(2).Info("starting edge-mc syncer")
	kcpVersion := version.Get().GitVersion

	bootstrapConfig := rest.CopyConfig(cfg.UpstreamConfig)
	rest.AddUserAgent(bootstrapConfig, "edge-mc#syncer/"+kcpVersion)

	syncConfigClientSet, err := syncerclientset.NewForConfig(bootstrapConfig)
	if err != nil {
		return err
	}
	syncConfigClient := syncConfigClientSet.EdgeV1alpha1().EdgeSyncConfigs()
	// syncConfigInformerFactory to watch a certain syncConfig on upstream
	syncConfigInformerFactory := syncerinformers.NewSharedScopedInformerFactoryWithOptions(syncConfigClientSet, resyncPeriod)
	syncConfigAccess := syncConfigInformerFactory.Edge().V1alpha1().EdgeSyncConfigs()

	syncConfigAccess.Lister().List(labels.Everything()) // TODO: Remove (for now, need to invoke List at once)

	syncConfigInformerFactory.Start(ctx.Done())
	syncConfigInformerFactory.WaitForCacheSync(ctx.Done())

	upstreamConfig := rest.CopyConfig(cfg.UpstreamConfig)
	rest.AddUserAgent(upstreamConfig, "edge-mc#syncer/"+kcpVersion)
	upstreamDynamicClient, err := dynamic.NewForConfig(upstreamConfig)
	if err != nil {
		return err
	}
	upstreamDiscoveryClient := discovery.NewDiscoveryClientForConfigOrDie(upstreamConfig)
	upstreamClientFactory, err := syncers.NewClientFactory(logger, upstreamDynamicClient, upstreamDiscoveryClient)
	if err != nil {
		return err
	}

	downstreamConfig := rest.CopyConfig(cfg.DownstreamConfig)
	rest.AddUserAgent(downstreamConfig, "edge-mc#syncer/"+kcpVersion)
	downstreamDynamicClient, err := dynamic.NewForConfig(downstreamConfig)
	if err != nil {
		return err
	}
	downstreamDiscoveryClient := discovery.NewDiscoveryClientForConfigOrDie(downstreamConfig)
	downstreamClientFactory, err := syncers.NewClientFactory(logger, downstreamDynamicClient, downstreamDiscoveryClient)
	if err != nil {
		return err
	}

	upSyncer, err := syncers.NewUpSyncer(logger, upstreamClientFactory, downstreamClientFactory, []edgev1alpha1.EdgeSyncConfigResource{}, []edgev1alpha1.EdgeSynConversion{})
	if err != nil {
		return err
	}
	downSyncer, err := syncers.NewDownSyncer(logger, upstreamClientFactory, downstreamClientFactory, []edgev1alpha1.EdgeSyncConfigResource{}, []edgev1alpha1.EdgeSynConversion{})
	if err != nil {
		return err
	}

	controller, err := controller.NewSyncConfigController(logger, syncConfigClient, syncConfigAccess, upSyncer, downSyncer, 5*time.Second)
	if err != nil {
		return err
	}

	go controller.Run(ctx, numSyncerThreads)
	runSync(ctx, cfg, syncConfigAccess.Lister(), upSyncer, downSyncer)
	return nil
}

func runSync(ctx context.Context, cfg *SyncerConfig, syncConfigLister edgev1alpha1listers.EdgeSyncConfigLister, upSyncer *syncers.UpSyncer, downSyncer *syncers.DownSyncer) {
	logger := klog.FromContext(ctx)
	logger.V(2).Info("Start sync")
	interval := cfg.Interval
	if interval < minimumInterval {
		interval = defaultInterval
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.Tick(interval):
			logger.V(2).Info("Sync ")
			for _, resource := range controller.GetDownSyncedResources() {
				if err := downSyncer.SyncOne(resource, controller.GetConversions()); err != nil {
					logger.V(1).Info(fmt.Sprintf("failed to downsync %s.%s/%s (ns=%s)", resource.Kind, resource.Group, resource.Name, resource.Namespace))
				}
				if err := downSyncer.BackStatusOne(resource, controller.GetConversions()); err != nil {
					logger.V(1).Info(fmt.Sprintf("failed to status upsync %s.%s/%s (ns=%s)", resource.Kind, resource.Group, resource.Name, resource.Namespace))
				}
			}
			for _, resource := range controller.GetUpSyncedResources() {
				if err := upSyncer.SyncOne(resource, controller.GetConversions()); err != nil {
					logger.V(1).Info(fmt.Sprintf("failed to upsync %s.%s/%s (ns=%s)", resource.Kind, resource.Group, resource.Name, resource.Namespace))
				}
			}
		}
	}
}
