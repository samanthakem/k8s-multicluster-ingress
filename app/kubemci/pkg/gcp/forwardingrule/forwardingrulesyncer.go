// Copyright 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package forwardingrule

import (
	"fmt"
	"net/http"
	"reflect"
	"sort"

	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"

	"github.com/golang/glog"
	"k8s.io/apimachinery/pkg/util/diff"
	ingresslb "k8s.io/ingress-gce/pkg/loadbalancers"
	"k8s.io/ingress-gce/pkg/utils"

	utilsnamer "github.com/GoogleCloudPlatform/k8s-multicluster-ingress/app/kubemci/pkg/gcp/namer"
	"github.com/GoogleCloudPlatform/k8s-multicluster-ingress/app/kubemci/pkg/gcp/status"
)

const (
	httpDefaultPortRange  = "80-80"
	httpsDefaultPortRange = "443-443"
)

// ForwardingRuleSyncer manages GCP forwarding rules for multicluster GCP L7 load balancers.
type ForwardingRuleSyncer struct {
	namer *utilsnamer.Namer
	// Instance of ForwardingRuleProvider interface for calling GCE ForwardingRule APIs.
	// There is no separate ForwardingRuleProvider interface, so we use the bigger LoadBalancers interface here.
	frp ingresslb.LoadBalancers
}

func NewForwardingRuleSyncer(namer *utilsnamer.Namer, frp ingresslb.LoadBalancers) ForwardingRuleSyncerInterface {
	return &ForwardingRuleSyncer{
		namer: namer,
		frp:   frp,
	}
}

// Ensure this implements ForwardingRuleSyncerInterface.
var _ ForwardingRuleSyncerInterface = &ForwardingRuleSyncer{}

// EnsureHttpForwardingRule ensures that the required http forwarding rule exists.
// Does nothing if it exists already, else creates a new one.
// Stores the given list of clusters in the description field of forwarding rule to use it to generate status later.
func (s *ForwardingRuleSyncer) EnsureHttpForwardingRule(lbName, ipAddress, targetProxyLink string, clusters []string, forceUpdate bool) error {
	fmt.Println("Ensuring http forwarding rule")
	desiredFR, err := s.desiredForwardingRule(lbName, ipAddress, targetProxyLink, clusters)
	if err != nil {
		fmt.Println("Error getting desired forwarding rule:", err)
		return err
	}
	name := desiredFR.Name
	// Check if forwarding rule already exists.
	existingFR, err := s.frp.GetGlobalForwardingRule(name)
	if err == nil {
		fmt.Println("forwarding rule", name, "exists already. Checking if it matches our desired forwarding rule", name)
		glog.V(5).Infof("Existing forwarding rule:\n%+v\nDesired forwarding rule:\n%+v", existingFR, desiredFR)
		// Forwarding Rule with that name exists already. Check if it matches what we want.
		if forwardingRuleMatches(desiredFR, existingFR) {
			// Nothing to do. Desired forwarding rule exists already.
			fmt.Println("Desired forwarding rule exists already")
			return nil
		}
		if forceUpdate {
			fmt.Println("Updating existing forwarding rule", name, "to match the desired state")
			return s.updateForwardingRule(existingFR, desiredFR)
		} else {
			fmt.Println("Will not overwrite this differing Forwarding Rule without the --force flag")
			return fmt.Errorf("Will not overwrite Forwarding Rule without --force")
		}
	}
	glog.V(2).Infof("Got error %s while trying to get existing forwarding rule %s. Will try to create new one", err, name)
	// TODO: Handle non NotFound errors. We should create only if the error is NotFound.
	// Create the forwarding rule.
	return s.createForwardingRule(desiredFR)
}

func (s *ForwardingRuleSyncer) DeleteForwardingRules() error {
	// TODO(nikhiljindal): Also delete the https forwarding rule when we start creating it.
	name := s.namer.HttpForwardingRuleName()
	fmt.Println("Deleting forwarding rule", name)
	err := s.frp.DeleteGlobalForwardingRule(name)
	if err != nil {
		fmt.Println("error", err, "in deleting forwarding rule", name)
		return err
	}
	fmt.Println("forwarding rule", name, "deleted successfully")
	return nil
}

func (s *ForwardingRuleSyncer) GetLoadBalancerStatus(lbName string) (*status.LoadBalancerStatus, error) {
	// Fetch the http forwarding rule.
	// TODO(nikhiljindal): Try fetching the https rule as well, once we start creating them.
	name := s.namer.HttpForwardingRuleName()
	fr, err := s.frp.GetGlobalForwardingRule(name)
	if utils.IsHTTPErrorCode(err, http.StatusNotFound) {
		// We assume the load balancer does not exist until the forwarding rule exists.
		return nil, fmt.Errorf("Load balancer %s does not exist", lbName)
	}
	if err != nil {
		return nil, fmt.Errorf("error in fetching forwarding rule: %s. Cannot determine status without it.", err)
	}
	status, err := status.FromString(fr.Description)
	if err != nil {
		return nil, fmt.Errorf("error in parsing forwarding rule description %s. Cannot determine status without it.", err)
	}
	return status, nil
}

func (s *ForwardingRuleSyncer) updateForwardingRule(existingFR, desiredFR *compute.ForwardingRule) error {
	name := desiredFR.Name
	// We do not have an UpdateForwardingRule method.
	// If target proxy link is the only thing that is different, then we can call SetProxyForGlobalForwardingRule.
	// Else, we need to delete the existing rule and create a new one.
	if existingFR.IPAddress != desiredFR.IPAddress || existingFR.PortRange != desiredFR.PortRange ||
		existingFR.IPProtocol != desiredFR.IPProtocol || existingFR.Description != desiredFR.Description {
		fmt.Println("Deleting the existing forwarding rule", name, "and will create a new one")
		if err := utils.IgnoreHTTPNotFound(s.frp.DeleteGlobalForwardingRule(name)); err != nil {
			fmt.Println("Error deleting global forwarding rule:", err)
			return fmt.Errorf("error in deleting existing forwarding rule %s: %s", name, err)
		}
		return s.createForwardingRule(desiredFR)
	}
	// Update target proxy link in forwarding rule.
	err := s.frp.SetProxyForGlobalForwardingRule(name, desiredFR.Target)
	if err != nil {
		fmt.Println("Error setting proxy for forwarding rule. Target:", desiredFR.Target, "Error:", err)
		return err
	}
	fmt.Println("Forwarding rule", name, "updated successfully")
	return nil
}

func (s *ForwardingRuleSyncer) createForwardingRule(desiredFR *compute.ForwardingRule) error {
	name := desiredFR.Name
	fmt.Println("Creating forwarding rule", name)
	glog.V(5).Infof("Creating forwarding rule %v", desiredFR)
	err := s.frp.CreateGlobalForwardingRule(desiredFR)
	if err != nil {
		return err
	}
	fmt.Println("Forwarding rule", name, "created successfully")
	return nil
}

func forwardingRuleMatches(desiredFR, existingFR *compute.ForwardingRule) bool {
	existingFR.CreationTimestamp = ""
	existingFR.Id = 0
	existingFR.Kind = ""
	existingFR.SelfLink = ""
	existingFR.ServerResponse = googleapi.ServerResponse{}

	equal := reflect.DeepEqual(existingFR, desiredFR)
	if !equal {
		glog.V(5).Infof("Forwarding Rules differ:\n%v", diff.ObjectDiff(desiredFR, existingFR))
	} else {
		glog.V(2).Infof("Rules match.")
	}

	return equal
}

func (s *ForwardingRuleSyncer) desiredForwardingRule(lbName, ipAddress, targetProxyLink string, clusters []string) (*compute.ForwardingRule, error) {
	// Sort the clusters so we get a deterministic order.
	sort.Strings(clusters)
	status := status.LoadBalancerStatus{
		Description:      fmt.Sprintf("Http forwarding rule for kubernetes multicluster loadbalancer %s", lbName),
		LoadBalancerName: lbName,
		Clusters:         clusters,
		IPAddress:        ipAddress,
	}
	desc, err := status.ToString()
	if err != nil {
		return nil, fmt.Errorf("unexpected error in generating the description for forwarding rule: %s", err)
	}
	// Compute the desired forwarding rule.
	return &compute.ForwardingRule{
		Name:                s.namer.HttpForwardingRuleName(),
		Description:         desc,
		IPAddress:           ipAddress,
		Target:              targetProxyLink,
		PortRange:           httpDefaultPortRange,
		IPProtocol:          "TCP",
		LoadBalancingScheme: "EXTERNAL",
	}, nil
}
