// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"strings"
	"sync/atomic"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	kubesr "istio.io/istio/pilot/pkg/serviceregistry/kube"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/mcs"
)

type exportedService struct {
	namespacedName  types.NamespacedName
	discoverability map[host.Name]string
}

// serviceExportCache reads Kubernetes Multi-Cluster Services (MCS) ServiceExport resources in the
// cluster and generates discoverability policies for the endpoints.
type serviceExportCache interface {
	// EndpointDiscoverabilityPolicy returns the policy for Service endpoints residing within the current cluster.
	EndpointDiscoverabilityPolicy(svc *model.Service) model.EndpointDiscoverabilityPolicy

	// ExportedServices returns the list of services that are exported in this cluster. Used for debugging.
	ExportedServices() []exportedService
	Run(stop <-chan struct{})

	// HasSynced indicates whether the kube createClient has synced for the watched resources.
	HasSynced() bool

	// HasCRDInstalled indicates whether the serviceExport crd has been installed.
	HasCRDInstalled() bool
}

// newServiceExportCache creates a new serviceExportCache that observes the given cluster.
func newServiceExportCache(c *Controller) serviceExportCache {
	if features.EnableMCSServiceDiscovery {
		ec := &serviceExportCacheImpl{
			Controller:      c,
			serviceExportCh: make(chan struct{}),
		}
		c.crdWatcher.AddCallBack(ec.onCRDEvent)

		// Set the discoverability policy for the clusterset.local host.
		ec.clusterSetLocalPolicySelector = func(svc *model.Service) (policy model.EndpointDiscoverabilityPolicy) {
			// If the service is exported in this cluster, allow the endpoints in this cluster to be discoverable
			// anywhere in the mesh.
			if ec.isExported(namespacedNameForService(svc)) {
				return model.AlwaysDiscoverable
			}

			// Otherwise, endpoints are only discoverable from within the same cluster.
			return model.DiscoverableFromSameCluster
		}

		// Set the discoverability policy for the cluster.local host.
		if features.EnableMCSClusterLocal {
			// MCS cluster.local mode is enabled. Allow endpoints for the cluster.local host to be
			// discoverable only from within the same cluster.
			ec.clusterLocalPolicySelector = func(svc *model.Service) (policy model.EndpointDiscoverabilityPolicy) {
				return model.DiscoverableFromSameCluster
			}
		} else {
			// MCS cluster.local mode is not enabled, so requests to the cluster.local host are not confined
			// to the same cluster. Use the same discoverability policy as for clusterset.local.
			ec.clusterLocalPolicySelector = ec.clusterSetLocalPolicySelector
		}

		return ec
	}

	// MCS Service discovery is disabled. Use a placeholder cache.
	return disabledServiceExportCache{}
}

type discoverabilityPolicySelector func(*model.Service) model.EndpointDiscoverabilityPolicy

// serviceExportCache reads ServiceExport resources for a single cluster.
type serviceExportCacheImpl struct {
	*Controller

	serviceExports kclient.Untyped

	// clusterLocalPolicySelector selects an appropriate EndpointDiscoverabilityPolicy for the cluster.local host.
	clusterLocalPolicySelector discoverabilityPolicySelector

	// clusterSetLocalPolicySelector selects an appropriate EndpointDiscoverabilityPolicy for the clusterset.local host.
	clusterSetLocalPolicySelector discoverabilityPolicySelector

	serviceExportCh chan struct{}

	started atomic.Bool
}

func (ec *serviceExportCacheImpl) onServiceExportEvent(_, obj controllers.Object, event model.Event) error {
	se := controllers.Extract[*unstructured.Unstructured](obj)
	if se == nil {
		return nil
	}

	switch event {
	case model.EventAdd, model.EventDelete:
		ec.updateXDS(se)
	default:
		// Don't care about updates.
	}
	return nil
}

func (ec *serviceExportCacheImpl) updateXDS(se metav1.Object) {
	for _, svc := range ec.servicesForNamespacedName(config.NamespacedName(se)) {
		// Re-build the endpoints for this service with a new discoverability policy.
		// Also update any internal caching.
		endpoints := ec.buildEndpointsForService(svc, true)
		shard := model.ShardKeyFromRegistry(ec)
		ec.opts.XDSUpdater.EDSUpdate(shard, svc.Hostname.String(), se.GetNamespace(), endpoints)
	}
}

func (ec *serviceExportCacheImpl) EndpointDiscoverabilityPolicy(svc *model.Service) model.EndpointDiscoverabilityPolicy {
	if !ec.started.Load() {
		return nil
	}
	if svc == nil {
		// Default policy when the service doesn't exist.
		return model.DiscoverableFromSameCluster
	}

	if strings.HasSuffix(svc.Hostname.String(), "."+constants.DefaultClusterSetLocalDomain) {
		return ec.clusterSetLocalPolicySelector(svc)
	}

	return ec.clusterLocalPolicySelector(svc)
}

func (ec *serviceExportCacheImpl) isExported(name types.NamespacedName) bool {
	return ec.serviceExports.Get(name.Name, name.Namespace) != nil
}

func (ec *serviceExportCacheImpl) ExportedServices() []exportedService {
	if !ec.started.Load() {
		return nil
	}
	// List all exports in this cluster.
	exports := ec.serviceExports.List(metav1.NamespaceAll, klabels.Everything())

	ec.RLock()

	out := make([]exportedService, 0, len(exports))
	for _, export := range exports {
		uExport := export.(*unstructured.Unstructured)
		es := exportedService{
			namespacedName:  config.NamespacedName(uExport),
			discoverability: make(map[host.Name]string),
		}

		// Generate the map of all hosts for this service to their discoverability policies.
		clusterLocalHost := kubesr.ServiceHostname(uExport.GetName(), uExport.GetNamespace(), ec.opts.DomainSuffix)
		clusterSetLocalHost := serviceClusterSetLocalHostname(es.namespacedName)
		for _, hostName := range []host.Name{clusterLocalHost, clusterSetLocalHost} {
			if svc := ec.servicesMap[hostName]; svc != nil {
				es.discoverability[hostName] = ec.EndpointDiscoverabilityPolicy(svc).String()
			}
		}

		out = append(out, es)
	}

	ec.RUnlock()

	return out
}

func (ec *serviceExportCacheImpl) Run(stop <-chan struct{}) {
	select {
	case <-ec.serviceExportCh:
	case <-stop:
		return
	}
	ec.serviceExports = kclient.NewDynamic(ec.client, mcs.ServiceExportGVR, kclient.Filter{ObjectFilter: ec.opts.GetFilter()})
	// Register callbacks for events.
	registerHandlers(ec.Controller, ec.serviceExports, "ServiceExports", ec.onServiceExportEvent, nil)
	ec.serviceExports.Start(stop)
	kube.WaitForCacheSync("service export", stop, ec.serviceExports.HasSynced)
	ec.started.Store(true)
}

func (ec *serviceExportCacheImpl) HasSynced() bool {
	return ec.started.Load()
}

func (ec *serviceExportCacheImpl) HasCRDInstalled() bool {
	select {
	case <-ec.serviceExportCh:
		return true
	default:
		return false
	}
}

func (ec *serviceExportCacheImpl) onCRDEvent(name string) {
	if name == mcs.ServiceExportGVR.Resource+"."+mcs.ServiceExportGVR.Group {
		select {
		case <-ec.serviceExportCh: // channel already closed
		default:
			// notify CRD added
			close(ec.serviceExportCh)
		}
	}
}

type disabledServiceExportCache struct{}

var _ serviceExportCache = disabledServiceExportCache{}

func (c disabledServiceExportCache) EndpointDiscoverabilityPolicy(*model.Service) model.EndpointDiscoverabilityPolicy {
	return model.AlwaysDiscoverable
}

func (c disabledServiceExportCache) Run(stop <-chan struct{}) {}

func (c disabledServiceExportCache) HasSynced() bool {
	return true
}

func (c disabledServiceExportCache) ExportedServices() []exportedService {
	// MCS is disabled - returning `nil`, which is semantically different here than an empty list.
	return nil
}

func (c disabledServiceExportCache) HasCRDInstalled() bool {
	return false
}
