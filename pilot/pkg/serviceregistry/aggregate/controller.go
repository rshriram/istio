// Copyright 2017 Istio Authors
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

package aggregate

import (
	"sync"

	"istio.io/istio/pilot/pkg/features"

	"github.com/hashicorp/go-multierror"

	"istio.io/pkg/log"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/labels"
)

var (
	clusterAddressesMutex sync.Mutex
)

// The aggregate controller does not implement serviceregistry.Instance since it may be comprised of various
// providers and clusters.
var _ model.ServiceDiscovery = &Controller{}
var _ model.Controller = &Controller{}

// Controller aggregates data across different registries and monitors for changes
type Controller struct {
	numK8SRegistries int
	registries       []serviceregistry.Instance
	storeLock        sync.RWMutex
}

// NewController creates a new Aggregate controller
func NewController() *Controller {
	return &Controller{
		registries: make([]serviceregistry.Instance, 0),
	}
}

// AddRegistry adds registries into the aggregated controller
func (c *Controller) AddRegistry(registry serviceregistry.Instance) {
	c.storeLock.Lock()
	defer c.storeLock.Unlock()

	c.registries = append(c.registries, registry)

	// Make a pass to determine if we have multiple kubernetes clusters connected to
	// this istiod or not. This will simplify logic for Services() and GetService()
	var numK8SClusters int
	for _, r := range c.GetRegistries() {
		// Assumes that clusters with named IDs are for kubernetes
		// which is the case today.
		if r.Cluster() != "" {
			numK8SClusters++
		}
	}
	c.numK8SRegistries = numK8SClusters
}

// DeleteRegistry deletes specified registry from the aggregated controller
func (c *Controller) DeleteRegistry(clusterID string) {
	c.storeLock.Lock()
	defer c.storeLock.Unlock()

	if len(c.registries) == 0 {
		log.Warnf("Registry list is empty, nothing to delete")
		return
	}
	index, ok := c.GetRegistryIndex(clusterID)
	if !ok {
		log.Warnf("Registry is not found in the registries list, nothing to delete")
		return
	}
	registries := c.registries
	registries = append(registries[:index], registries[index+1:]...)
	c.registries = registries
	// Make a pass to determine if we have multiple kubernetes clusters connected to
	// this istiod or not. This will simplify logic for Services() and GetService()
	var numK8SClusters int
	for _, r := range c.GetRegistries() {
		// Assumes that clusters with named IDs are for kubernetes
		// which is the case today.
		if r.Cluster() != "" {
			numK8SClusters++
		}
	}
	c.numK8SRegistries = numK8SClusters
	log.Infof("Registry for the cluster %s has been deleted.", clusterID)
}

// GetRegistries returns a copy of all registries
func (c *Controller) GetRegistries() []serviceregistry.Instance {
	c.storeLock.RLock()
	defer c.storeLock.RUnlock()

	return c.registries
}

// GetRegistryIndex returns the index of a registry
func (c *Controller) GetRegistryIndex(clusterID string) (int, bool) {
	for i, r := range c.registries {
		if r.Cluster() == clusterID {
			return i, true
		}
	}
	return 0, false
}

// Services lists services from all platforms
func (c *Controller) Services() ([]*model.Service, error) {
	// smap is a map of hostname (string) to service, used to identify kubernetes
	// services that are installed in multiple clusters. The smap contains a complete copy of
	// the service object to eliminate unnecessary locking and race conditions. Map fields such as
	// ClusterVIPs, ClusterExternalAddresses, etc. are merged across copies of the service from
	// different clusters.
	smap := make(map[host.Name]*model.Service)
	services := make([]*model.Service, 0)
	var errs error

	if c.numK8SRegistries <= 1 {
		for _, r := range c.GetRegistries() {
			// We also assume that among multiple registries of different types, there are no
			// duplicate service names.
			svcs, err := r.Services()
			if err != nil {
				errs = multierror.Append(errs, err)
				continue
			}
			services = append(services, svcs...)
		}
		return services, errs
	}

	// If we have multiple kubernetes clusters being managed by same istiod,
	// we need to build a new canonical copy of services that has merged data
	// from all underlying clusters.

	// Locking Registries list while walking it to prevent inconsistent results
	for _, r := range c.GetRegistries() {
		svcs, err := r.Services()
		if err != nil {
			errs = multierror.Append(errs, err)
			continue
		}
		// Race condition: multiple threads may call Services, and multiple services
		// may modify one of the service's cluster ID
		clusterAddressesMutex.Lock()
		if r.Cluster() == "" { // Should we instead check for registry name to be on safe side?
			// If the service does not have a cluster ID (consul, ServiceEntries, CloudFoundry, etc.)
			// Do not bother checking for the cluster ID.
			// DO NOT ASSIGN CLUSTER ID to non-k8s registries. This will prevent service entries with multiple
			// VIPs or CIDR ranges in the address field
			services = append(services, svcs...)
		} else {
			// This is K8S typically
			for _, s := range svcs {
				sp, ok := smap[s.Hostname]
				if !ok {
					// First time we see a service. Make a copy to prevent modifying the underlying registry's copy.
					sp = s.DeepCopy()
					smap[s.Hostname] = sp
					services = append(services, sp)
				}

				// to avoid unnecessary locking, obtain a reference to the required maps
				// this way, even if the underlying controller replaces the map, this code would be seeing
				// an older version of the data, without any concurrency issues (:fingers crossed that go compiler treats
				// map object assignment as pointer updates :)
				clusterExternalAddresses := s.Attributes.ClusterExternalAddresses
				clusterExternalPorts := s.Attributes.ClusterExternalPorts

				if clusterExternalAddresses != nil {
					sp.Attributes.ClusterExternalAddresses[r.Cluster()] = clusterExternalAddresses[r.Cluster()]
				}
				if clusterExternalPorts != nil {
					sp.Attributes.ClusterExternalPorts[r.Cluster()] = clusterExternalPorts[r.Cluster()]
				}
				sp.ClusterVIPs[r.Cluster()] = s.Address
			}
		}
		clusterAddressesMutex.Unlock()
	}
	return services, errs
}

// GetService retrieves a service by hostname if exists
// Currently only used to get get gateway service
// TODO: merge with Services()
func (c *Controller) GetService(hostname host.Name) (*model.Service, error) {
	var errs error
	var out *model.Service
	if c.numK8SRegistries <= 1 {
		for _, r := range c.GetRegistries() {
			service, err := r.GetService(hostname)
			if err != nil {
				errs = multierror.Append(errs, err)
				continue
			}
			if service == nil {
				continue
			}
			return service, nil
		}
		return nil, errs
	}

	for _, r := range c.GetRegistries() {
		service, err := r.GetService(hostname)
		if err != nil {
			errs = multierror.Append(errs, err)
			continue
		}
		if service == nil {
			continue
		}
		if r.Cluster() == "" { // Should we instead check for registry name to be on safe side?
			// If the service does not have a cluster ID (consul, ServiceEntries, CloudFoundry, etc.)
			// Do not bother checking for the cluster ID.
			// DO NOT ASSIGN CLUSTER ID to non-k8s registries. This will prevent service entries with multiple
			// VIPs or CIDR ranges in the address field
			return service, nil
		}
		// This is K8S typically
		if out == nil {
			out = service.DeepCopy()
		} else {
			// to avoid unnecessary locking, obtain a reference to the required maps
			// this way, even if the underlying controller replaces the map, this code would be seeing
			// an older version of the data, without any concurrency issues (:fingers crossed that go compiler treats
			// map object assignment as pointer updates :)
			clusterExternalAddresses := service.Attributes.ClusterExternalAddresses
			clusterExternalPorts := service.Attributes.ClusterExternalPorts

			if clusterExternalAddresses != nil {
				out.Attributes.ClusterExternalAddresses[r.Cluster()] = clusterExternalAddresses[r.Cluster()]
			}
			if clusterExternalPorts != nil {
				out.Attributes.ClusterExternalPorts[r.Cluster()] = clusterExternalPorts[r.Cluster()]
			}
			out.ClusterVIPs[r.Cluster()] = service.Address
		}
	}
	return out, errs
}

// ManagementPorts retrieves set of health check ports by instance IP
// Return on the first hit.
func (c *Controller) ManagementPorts(addr string) model.PortList {
	for _, r := range c.GetRegistries() {
		if portList := r.ManagementPorts(addr); portList != nil {
			return portList
		}
	}
	return nil
}

// WorkloadHealthCheckInfo returne the health check information for IP addr
// Return on the first hit.
func (c *Controller) WorkloadHealthCheckInfo(addr string) model.ProbeList {
	for _, r := range c.GetRegistries() {
		if probeList := r.WorkloadHealthCheckInfo(addr); probeList != nil {
			return probeList
		}
	}
	return nil
}

// InstancesByPort retrieves instances for a service on a given port that match
// any of the supplied labels. All instances match an empty label list.
func (c *Controller) InstancesByPort(svc *model.Service, port int,
	labels labels.Collection) ([]*model.ServiceInstance, error) {
	var instances, tmpInstances []*model.ServiceInstance
	var errs error
	for _, r := range c.GetRegistries() {
		var err error
		tmpInstances, err = r.InstancesByPort(svc, port, labels)
		if err != nil {
			errs = multierror.Append(errs, err)
		} else if len(tmpInstances) > 0 {
			if errs != nil {
				log.Warnf("Instances() found match but encountered an error: %v", errs)
			}
			instances = append(instances, tmpInstances...)
		}
	}
	if len(instances) > 0 {
		errs = nil
	}
	return instances, errs
}

func nodeClusterID(node *model.Proxy) string {
	if node.Metadata == nil || node.Metadata.ClusterID == "" {
		return ""
	}
	return node.Metadata.ClusterID
}

// Skip the service registry when there won't be a match
// because the proxy is in a different cluster.
func skipSearchingRegistryForProxy(nodeClusterID, registryClusterID, selfClusterID string) bool {
	// We can't trust the default service registry because its always
	// named `Kubernetes`. Use the `CLUSTER_ID` envvar to find the
	// local cluster name in these cases.
	// TODO(https://github.com/istio/istio/issues/22093)
	if registryClusterID == string(serviceregistry.Kubernetes) {
		registryClusterID = selfClusterID
	}

	// We can't be certain either way
	if registryClusterID == "" || nodeClusterID == "" {
		return false
	}

	return registryClusterID != nodeClusterID
}

// GetProxyServiceInstances lists service instances co-located with a given proxy
func (c *Controller) GetProxyServiceInstances(node *model.Proxy) ([]*model.ServiceInstance, error) {
	out := make([]*model.ServiceInstance, 0)
	var errs error
	// It doesn't make sense for a single proxy to be found in more than one registry.
	// TODO: if otherwise, warning or else what to do about it.
	for _, r := range c.GetRegistries() {
		nodeClusterID := nodeClusterID(node)
		if skipSearchingRegistryForProxy(nodeClusterID, r.Cluster(), features.ClusterName.Get()) {
			log.Debugf("GetProxyServiceInstances(): not searching registry %v: proxy %v CLUSTER_ID is %v",
				r.Cluster(), node.ID, nodeClusterID)
			continue
		}

		instances, err := r.GetProxyServiceInstances(node)
		if err != nil {
			errs = multierror.Append(errs, err)
		} else if len(instances) > 0 {
			out = append(out, instances...)
			node.ClusterID = instances[0].Endpoint.Locality.ClusterID
			break
		}
	}

	if len(out) > 0 {
		if errs != nil {
			log.Debugf("GetProxyServiceInstances() found match but encountered an error: %v", errs)
		}
		return out, nil
	}

	return out, errs
}

func (c *Controller) GetProxyWorkloadLabels(proxy *model.Proxy) (labels.Collection, error) {
	var out labels.Collection
	var errs error
	// It doesn't make sense for a single proxy to be found in more than one registry.
	// TODO: if otherwise, warning or else what to do about it.
	for _, r := range c.GetRegistries() {
		wlLabels, err := r.GetProxyWorkloadLabels(proxy)
		if err != nil {
			errs = multierror.Append(errs, err)
		} else if len(wlLabels) > 0 {
			out = append(out, wlLabels...)
			break
		}
	}

	if len(out) > 0 {
		if errs != nil {
			log.Warnf("GetProxyWorkloadLabels() found match but encountered an error: %v", errs)
		}
		return out, nil
	}

	return out, errs
}

// Run starts all the controllers
func (c *Controller) Run(stop <-chan struct{}) {

	for _, r := range c.GetRegistries() {
		go r.Run(stop)
	}

	<-stop
	log.Info("Registry Aggregator terminated")
}

// AppendServiceHandler implements a service catalog operation
func (c *Controller) AppendServiceHandler(f func(*model.Service, model.Event)) error {
	for _, r := range c.GetRegistries() {
		if err := r.AppendServiceHandler(f); err != nil {
			log.Infof("Fail to append service handler to adapter %s", r.Provider())
			return err
		}
	}
	return nil
}

// AppendInstanceHandler implements a service instance catalog operation
func (c *Controller) AppendInstanceHandler(f func(*model.ServiceInstance, model.Event)) error {
	for _, r := range c.GetRegistries() {
		if err := r.AppendInstanceHandler(f); err != nil {
			log.Infof("Fail to append instance handler to adapter %s", r.Provider())
			return err
		}
	}
	return nil
}

// GetIstioServiceAccounts implements model.ServiceAccounts operation
func (c *Controller) GetIstioServiceAccounts(svc *model.Service, ports []int) []string {
	for _, r := range c.GetRegistries() {
		if svcAccounts := r.GetIstioServiceAccounts(svc, ports); svcAccounts != nil {
			return svcAccounts
		}
	}
	return nil
}
