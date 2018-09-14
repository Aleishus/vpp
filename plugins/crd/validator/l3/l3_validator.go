// Copyright (c) 2018 Cisco and/or its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package l3

import (
	"fmt"
	"github.com/contiv/vpp/plugins/crd/api"
	"github.com/contiv/vpp/plugins/crd/cache/telemetrymodel"
	"github.com/contiv/vpp/plugins/crd/datastore"
	"github.com/contiv/vpp/plugins/crd/validator/utils"

	"github.com/ligato/cn-infra/logging"

	"regexp"
	"strings"
)

const (
	routeNotValidated = iota
	routeInvalid      = iota
	routeValid        = iota
)

// Validator is the implementation of the ContivTelemetryProcessor interface.
type Validator struct {
	Log logging.Logger

	VppCache api.VppCache
	K8sCache api.K8sCache
	Report   api.Report
}

// Vrf is a type declaration to help simplify a map of maps
type Vrf map[string]telemetrymodel.NodeIPRoute

// VrfMap keeps the routing table organized by VRF IDs
type VrfMap map[uint32]Vrf

// RouteMap defines the structure for keeping track of validated/valid/invalid
// routes
type RouteMap map[uint32]map[string]int

//Validate will validate each nodes and pods l3 connectivity for any errors
func (v *Validator) Validate() {
	nodeList := v.VppCache.RetrieveAllNodes()
	numErrs := 0

	for _, node := range nodeList {

		vrfMap, err := v.createVrfMap(node)
		if err != nil {
			v.Report.LogErrAndAppendToNodeReport(node.Name, err.Error())
		}
		routeMap := v.createValidationMap(vrfMap)

		// Validate routes to local pods (they are all on vrf 1).
		numErrs += v.validateVrf1PodRoutes(node, vrfMap, routeMap)

		// Validate the vrf1 route to the local loop interface
		numErrs += v.validateRouteToLocalLoopInterface(node, vrfMap, routeMap)

		// Validate local nodes gigE routes
		numErrs += v.validateVrf0GigERoutes(node, vrfMap, routeMap)

		// Validate vrf 0 local routes
		numErrs += v.validateVrf0LocalHostRoute(node, vrfMap, routeMap)

		// Validate vrf 1 default route
		numErrs += v.validateDefaultRoutes(node, vrfMap, routeMap)

		// Validate routes to all remote nodes for vrf 1 and vrf 0
		numErrs += v.validateRemoteNodeRoutes(node, vrfMap, routeMap)

		// Validate podSubnetCIDR routes
		numErrs += v.validatePodSubnetCidrRoutes(node, vrfMap, routeMap)

		// Validate podSubnetCIDR routes
		numErrs += v.validateVppHostNetworkRoutes(node, vrfMap, routeMap)

		for vIdx, vrf := range routeMap {
			var notValidated, invalid, valid int

			for _, rteStatus := range vrf {
				switch rteStatus {
				case routeNotValidated:
					notValidated++
				case routeInvalid:
					invalid++
				case routeValid:
					valid++
				}
			}

			report := fmt.Sprintf("Rte report VRF%d: total %d, notValidated %d, invalid: %d, valid:%d",
				vIdx, len(vrf), notValidated, invalid, valid)
			v.Report.AppendToNodeReport(node.Name, report)
		}

		fmt.Println(node.Name + ":")
		printValidationMap(routeMap)
	}

	if numErrs == 0 {
		v.Report.AppendToNodeReport(api.GlobalMsg, "success validating l3 info.")
	} else {
		errString := fmt.Sprintf("%d Errors in L3 validation...", numErrs)
		v.Report.AppendToNodeReport(api.GlobalMsg, errString)
	}
}

func (v *Validator) createVrfMap(node *telemetrymodel.Node) (VrfMap, error) {
	vrfMap := make(VrfMap, 0)
	for _, route := range node.NodeStaticRoutes {
		vrf, ok := vrfMap[route.Ipr.VrfID]
		if !ok {
			vrfMap[route.Ipr.VrfID] = make(Vrf, 0)
			vrf = vrfMap[route.Ipr.VrfID]
		}

		if !strings.Contains(route.IprMeta.TableName, "-VRF:") {
			continue
		}
		vrf[route.Ipr.DstAddr] = route
	}
	return vrfMap, nil
}

func (v *Validator) createValidationMap(vm map[uint32]Vrf) RouteMap {
	valMap := make(RouteMap, 0)

	for vIdx, vrf := range vm {
		vrfRoutes := make(map[string]int, 0)
		for _, rte := range vrf {
			vrfRoutes[rte.Ipr.DstAddr] = routeNotValidated
		}
		valMap[vIdx] = vrfRoutes
	}

	return valMap
}

func (v *Validator) validateVrf1PodRoutes(node *telemetrymodel.Node, vrfMap VrfMap, routeMap RouteMap) int {

	numErrs := 0
	for _, pod := range node.PodMap {

		// Skip over host network pods
		if pod.IPAddress == node.ManIPAddr {
			continue
		}

		// Validate routes to local Pods
		// Lookup the Pod route in VRF1; it must have mask length = 32
		numErrs += v.validateRoute(pod.IPAddress+"/32", 1, vrfMap, routeMap, node.Name,
			pod.VppIfName, pod.VppSwIfIdx, pod.IPAddress, 0)

		// make sure pod that the route for the pod-facing tap interface in vpp
		// exists and is valid
		numErrs += v.validateRoute(pod.VppIfIPAddr, 1, vrfMap, routeMap, node.Name,
			pod.VppIfName, pod.VppSwIfIdx, strings.Split(pod.VppIfIPAddr, "/")[0], 0)
	}

	return numErrs
}

func (v *Validator) validateVrf0GigERoutes(node *telemetrymodel.Node, vrfMap VrfMap, routeMap RouteMap) int {
	numErrs := 0

	ifc, found, err := findInterface(`GigabitEthernet[0-9]/[0-9]*/[0-9]`, node.NodeInterfaces)
	if err != nil {
		numErrs++
		errString := fmt.Sprintf("local GigE interface lookup match error %s", err)
		v.Report.LogErrAndAppendToNodeReport(node.Name, errString)
		return numErrs
	}

	if !found {
		numErrs++
		errString := fmt.Sprintf("route validation failed - local GigE interface not found")
		v.Report.LogErrAndAppendToNodeReport(node.Name, errString)
		return numErrs
	}

	// Validate the route to the local subnet
	numErrs += v.validateRoute(node.IPAddr, 0, vrfMap, routeMap, node.Name, ifc.If.Name,
		uint32(ifc.IfMeta.SwIfIndex), "0.0.0.0", 0)

	// Validate routes to all VPP nodes (remote and local) that are connected
	// to the GigE subnet
	nodeList := v.VppCache.RetrieveAllNodes()
	for _, node := range nodeList {
		dstIP, _ := separateIPandMask(node.IPAddr)
		numErrs += v.validateRoute(dstIP+"/32", 0, vrfMap, routeMap, node.Name, ifc.If.Name,
			uint32(ifc.IfMeta.SwIfIndex), dstIP, 0)
	}

	return numErrs
}

func (v *Validator) validateRemoteNodeRoutes(node *telemetrymodel.Node, vrfMap VrfMap, routeMap RouteMap) int {
	//validate remote nodes connectivity to current node
	numErrs := 0

	// Find local BVI - this will be the outgoing ifIndex for routes to
	// remote nodes
	localVxlanBVI, found, err := findInterface("vxlanBVI", node.NodeInterfaces)
	if err != nil {
		numErrs++
		errString := fmt.Sprintf("local vxlanBVI lookup failed, error %s", err)
		v.Report.LogErrAndAppendToNodeReport(node.Name, errString)
		return numErrs
	}

	if !found {
		numErrs++
		errString := fmt.Sprintf("local vxlanBVI not found")
		v.Report.LogErrAndAppendToNodeReport(node.Name, errString)
		return numErrs
	}

	// Validate routes for all *remote* nodes (i.e. skip over the local node)
	nodeList := v.VppCache.RetrieveAllNodes()
	for _, othNode := range nodeList {
		if othNode.Name == node.Name {
			continue
		}

		podNwIP := othNode.NodeIPam.PodNetwork
		route, ok := vrfMap[1][podNwIP]
		if !ok {
			errString := fmt.Sprintf("Route for pod network for node %s with ip %s not found",
				othNode.Name, podNwIP)
			v.Report.LogErrAndAppendToNodeReport(node.Name, errString)
			numErrs++
		}

		// Assume that the route will be valid. Each failed check flips
		// the status
		routeMap[1][route.Ipr.DstAddr] = routeValid

		//look for vxlanBD, make sure the route outgoing interface idx points to vxlanBVI
		for _, bd := range node.NodeBridgeDomains {
			if bd.Bd.Name == "vxlanBD" {
				if bd.BdMeta.BdID2Name[route.IprMeta.OutgoingIfIdx] != "vxlanBVI" {
					numErrs++
					routeMap[1][route.Ipr.DstAddr] = routeInvalid
					errString := fmt.Sprintf("vxlanBD outgoing interface for ipr index %d for route "+
						"with pod network ip %s is not vxlanBVI", route.IprMeta.OutgoingIfIdx, podNwIP)
					v.Report.LogErrAndAppendToNodeReport(node.Name, errString)
				}
			}
			for _, intf := range bd.Bd.Interfaces {
				if intf.Name == "vxlanBVI" {
					if !intf.BVI {
						numErrs++
						routeMap[1][route.Ipr.DstAddr] = routeInvalid
						errString := fmt.Sprintf("Bridge domain %s interface %s BVI is %+v, expected true",
							bd.Bd.Name, intf.Name, intf.BVI)
						v.Report.LogErrAndAppendToNodeReport(node.Name, errString)
					}
				}
			}
		}
		// Find the remote node vxlanBD, find the interface which the idx
		// points to, make sure that one of the ip addresses is the same as
		// the main node's route's next hop ip
		for _, bd := range othNode.NodeBridgeDomains {
			for id, name := range bd.BdMeta.BdID2Name {
				if name == "vxlanBVI" {
					intf := othNode.NodeInterfaces[int(id)]
					matchingIPFound := false
					for _, ip := range intf.If.IPAddresses {
						if ip == route.Ipr.NextHopAddr+"/24" {
							matchingIPFound = true
						}
					}
					if !matchingIPFound {
						numErrs++
						routeMap[1][route.Ipr.DstAddr] = routeInvalid
						errString := fmt.Sprintf("no matching ip found in remote node %s interface "+
							"%s to match current node %s route next hop %s",
							othNode.Name, intf.If.Name, node.Name, route.Ipr.NextHopAddr)
						v.Report.LogErrAndAppendToNodeReport(node.Name, errString)
					}
				}
			}
		}
		// validate routes to Host IP addresses (Management IP addresses) on
		// remote nodes in VRF0 (points to VRF1)
		numErrs += v.validateRoute(othNode.ManIPAddr+"/32", 0, vrfMap, routeMap, node.Name,
			"", 0, "0.0.0.0", 1)

		// validate routes to Host IP addresses (Management IP addresses) on
		// remote nodes in VRF0 (points to remote vxlanBVI IP addess, and going
		// out through the local vxlanBVI)
		ifc, found, err := findInterface("vxlanBVI", othNode.NodeInterfaces)
		if err != nil {
			numErrs++
			errString := fmt.Sprintf("failed to validate route %s VRF%d - "+
				"failed lookup for vxlanBVI for node %s, error %s", othNode.ManIPAddr+"/32", 0, othNode.Name, err)
			v.Report.LogErrAndAppendToNodeReport(node.Name, errString)
			continue
		}

		if !found {
			numErrs++
			errString := fmt.Sprintf("failed to validate route %s VRF%d - vxlanBVI for node %s not found",
				othNode.ManIPAddr+"/32", 0, othNode.Name)
			v.Report.LogErrAndAppendToNodeReport(node.Name, errString)
			continue
		}

		// KISS and assume for now that we only have a single IP address on
		// the BVI interface
		bviAddr := strings.Split(ifc.If.IPAddresses[0], "/")[0]

		numErrs += v.validateRoute(othNode.ManIPAddr+"/32", 1, vrfMap, routeMap, node.Name,
			"vxlanBVI", localVxlanBVI.IfMeta.SwIfIndex, bviAddr, 0)
	}

	return numErrs
}

func (v *Validator) validateVrf0LocalHostRoute(node *telemetrymodel.Node, vrfMap VrfMap, routeMap RouteMap) int {

	//validate local route to host and that the interface is correct
	numErrs := 0
	localRoute, ok := vrfMap[0][node.ManIPAddr+"/32"]
	if !ok {
		numErrs++
		errString := fmt.Sprintf("missing route with dst IP %s in VRF0 for node %s",
			node.ManIPAddr+"/32", node.Name)
		v.Report.LogErrAndAppendToNodeReport(node.Name, errString)
		return numErrs
	}

	tapIntf := node.NodeInterfaces[int(localRoute.IprMeta.OutgoingIfIdx)]
	var nextHop string
	for _, arpEntry := range node.NodeIPArp {
		if arpEntry.AeMeta.IfIndex == tapIntf.IfMeta.SwIfIndex {
			nextHop = arpEntry.Ae.IPAddress
			break
		}
	}

	return v.validateRoute(node.ManIPAddr+"/32", 0, vrfMap, routeMap, node.Name,
		"tap-vpp2", tapIntf.IfMeta.SwIfIndex, nextHop, 0)
}

func (v *Validator) validateDefaultRoutes(node *telemetrymodel.Node, vrfMap VrfMap, routeMap RouteMap) int {

	numErrs := 0

	// Validate the default route in VRF1
	numErrs += v.validateRoute("0.0.0.0/0", 1, vrfMap, routeMap, node.Name,
		"", 0, "0.0.0.0", 0)

	// Validate the default route in VRF0:
	// - It must point to the GigE interface, so find its ifIndex
	// - If we know the next hop (from th ARP table), use it, otherwise do
	//   not validate the next hop
	ifc, found, err := findInterface(`GigabitEthernet[0-9]/[0-9]*/[0-9]`, node.NodeInterfaces)
	if err != nil {
		numErrs++
		errString := fmt.Sprintf("failed to validate route %s VRF%d - "+
			"local GigE interface lookup match error %s", "0.0.0.0/0", 0, err)
		v.Report.LogErrAndAppendToNodeReport(node.Name, errString)
		return numErrs
	}

	if !found {
		numErrs++
		errString := fmt.Sprintf("failed to validate route %s VRF%d - local GigE interface not found",
			"0.0.0.0/0", 0)
		v.Report.LogErrAndAppendToNodeReport(node.Name, errString)
		return numErrs
	}

	var nextHop string
	for _, arpEntry := range node.NodeIPArp {
		if arpEntry.AeMeta.IfIndex == ifc.IfMeta.SwIfIndex {
			nextHop = arpEntry.Ae.IPAddress
			break
		}
	}

	numErrs += v.validateRoute("0.0.0.0/0", 0, vrfMap, routeMap, node.Name,
		"", ifc.IfMeta.SwIfIndex, nextHop, 0)

	return numErrs
}

func (v *Validator) validateRouteToLocalLoopInterface(node *telemetrymodel.Node, vrfMap map[uint32]Vrf,
	routeMap map[uint32]map[string]int) int {

	numErrs := 0
	loopIf, err := datastore.GetNodeLoopIFInfo(node)
	if err != nil {
		numErrs++
		v.Report.LogErrAndAppendToNodeReport(node.Name, err.Error())
		return numErrs
	}

	//validateRouteToLocalNodeLoopInterface
	for _, ip := range loopIf.If.IPAddresses {
		numErrs += v.validateRoute(ip, 1, vrfMap, routeMap, node.Name,
			loopIf.IfMeta.Tag, loopIf.IfMeta.SwIfIndex, "0.0.0.0", 0)
	}
	return numErrs
}

func (v *Validator) validatePodSubnetCidrRoutes(node *telemetrymodel.Node, vrfMap VrfMap, routeMap RouteMap) int {
	numErrs := 0

	podSubnetCidrRte := node.NodeIPam.Config.PodSubnetCIRDR

	numErrs += v.validateRoute(podSubnetCidrRte, 0, vrfMap, routeMap, node.Name,
		"", 0, "0.0.0.0", 1)
	numErrs += v.validateRoute(podSubnetCidrRte, 1, vrfMap, routeMap, node.Name,
		"local0", 0, "0.0.0.0", 0)
	return numErrs
}

func (v *Validator) validateVppHostNetworkRoutes(node *telemetrymodel.Node, vrfMap VrfMap, routeMap RouteMap) int {
	numErrs := 0

	numErrs += v.validateRoute(node.NodeIPam.Config.VppHostSubnetCIDR, 0, vrfMap, routeMap, node.Name,
		"", 0, "0.0.0.0", 1)
	numErrs += v.validateRoute(node.NodeIPam.Config.VppHostSubnetCIDR, 1, vrfMap, routeMap, node.Name,
		"local0", 0, "0.0.0.0", 0)

	numErrs += v.validateLocalVppHostNetworkRoute(node, vrfMap, routeMap)
	/*
		hostNetworkAddr, hostNetworkMask, err := utils.Ipv4CidrToAddressAndMask(node.NodeIPam.VppHostNetwork)
		if err != nil {
			numErrs++
			errString := fmt.Sprintf("failed to valide route %s in both VRFs; bad CIDR format, err %s",
				node.NodeIPam.VppHostNetwork, err)
			v.Report.LogErrAndAppendToNodeReport(node.Name, errString)
			return numErrs
		}

		hostSubnetAddr, hostSubnetMask, err := utils.Ipv4CidrToAddressAndMask(node.NodeIPam.Config.VppHostSubnetCIDR)
		if err != nil {
			numErrs++
			errString := fmt.Sprintf("failed to valide VppHostSubnetCIDR; have %s, err %s",
				node.NodeIPam.Config.VppHostSubnetCIDR, err)
			v.Report.LogErrAndAppendToNodeReport(node.Name, errString)
			return numErrs
		}
	*/
	return numErrs
}

func (v *Validator) validateLocalVppHostNetworkRoute(node *telemetrymodel.Node, vrfMap VrfMap, routeMap RouteMap) int {
	numErrs := 0

	ifc, found, err := findInterface(`tap-vpp2`, node.NodeInterfaces)
	if err != nil {
		numErrs++
		errString := fmt.Sprintf("failed to validate route to tap-vpp2 - "+
			"failed lookup for tap-vpp2, err %s", err)
		v.Report.LogErrAndAppendToNodeReport(node.Name, errString)
		return numErrs
	}

	if !found {
		numErrs++
		errString := fmt.Sprintf("failed to validate route to local host subnet - tap-vpp2 not found")
		v.Report.LogErrAndAppendToNodeReport(node.Name, errString)
		return numErrs

	}

	numErrs += v.validateRoute(ifc.If.IPAddresses[0], 0, vrfMap, routeMap, node.Name,
		"", ifc.IfMeta.SwIfIndex, "0.0.0.0", 0)

	// Make sure that the tap-vpp2 ip address is within the vppHostNetwork subnet
	ifHostNetAddr, ifHostNetMask, err := utils.Ipv4CidrToAddressAndMask(ifc.If.IPAddresses[0])
	if err != nil {
		numErrs++
		errString := fmt.Sprintf("tap-vpp2 IP address %s bad format; err %s",
			ifc.If.IPAddresses[0], err)
		v.Report.LogErrAndAppendToNodeReport(node.Name, errString)
		return numErrs
	}
	ifHostNetPrefix := ifHostNetAddr &^ ifHostNetMask

	ipamHostNetAddr, ipamHostNetMask, err := utils.Ipv4CidrToAddressAndMask(node.NodeIPam.VppHostNetwork)
	if err != nil {
		numErrs++
		errString := fmt.Sprintf("ipam vppHostNetwork %s bad format; err %s",
			node.NodeIPam.VppHostNetwork, err)
		v.Report.LogErrAndAppendToNodeReport(node.Name, errString)
		return numErrs
	}
	ipamHostNetPrefix := ipamHostNetAddr &^ ipamHostNetMask

	if (ifHostNetMask != ipamHostNetMask) || (ifHostNetPrefix != ipamHostNetPrefix) {
		numErrs++
		errString := fmt.Sprintf("inconsisten ipam vppHostNetwork %s vs tap-vpp2 IP address %s",
			node.NodeIPam.VppHostNetwork, ifc.If.IPAddresses[0])
		v.Report.LogErrAndAppendToNodeReport(node.Name, errString)
		return numErrs
	}

	drop1Addr := utils.AddressAndMaskToIPv4(ifHostNetAddr, ^uint32(0))
	numErrs += v.validateRoute(drop1Addr, 0, vrfMap, routeMap, node.Name,
		"", ifc.IfMeta.SwIfIndex, strings.Split(ifc.If.IPAddresses[0], "/")[0], 0)

	drop2Addr := utils.AddressAndMaskToIPv4(ifHostNetPrefix, ^uint32(0))
	numErrs += v.validateRoute(drop2Addr, 0, vrfMap, routeMap, node.Name,
		"", 0, "0.0.0.0", 0)

	drop3Addr := utils.AddressAndMaskToIPv4(ifHostNetPrefix+ifHostNetMask, ^uint32(0))
	numErrs += v.validateRoute(drop3Addr, 0, vrfMap, routeMap, node.Name,
		"", 0, "0.0.0.0", 0)

	return numErrs
}

// validateRoute performs all validations checks on a given route
func (v *Validator) validateRoute(rteID string, vrfID uint32, vrfMap VrfMap, rtMap RouteMap, nodeName string,
	eOutIface string, eOutgoingIfIdx uint32, eNextHopAddr string, eViaVrf uint32) int {

	numErrs := 0

	route, ok := vrfMap[vrfID][rteID]
	if !ok {
		numErrs++
		errString := fmt.Sprintf("missing route to PodSubnet %s in VRF%d", rteID, vrfID)
		v.Report.LogErrAndAppendToNodeReport(nodeName, errString)

		return numErrs
	}

	rtMap[vrfID][route.Ipr.DstAddr] = routeValid

	matched, err := regexp.Match(eOutIface, []byte(route.Ipr.OutIface))
	if err != nil {
		numErrs++
		rtMap[vrfID][route.Ipr.DstAddr] = routeInvalid
		errString := fmt.Sprintf("failed to match route %s outgoing interface (ifName %s) in VRF%d",
			route.Ipr.DstAddr, route.Ipr.OutIface, vrfID)
		v.Report.LogErrAndAppendToNodeReport(nodeName, errString)
	} else if !matched {
		numErrs++
		rtMap[vrfID][route.Ipr.DstAddr] = routeInvalid
		errString := fmt.Sprintf("invalid route %s in VRF%d; bad outgoing if - "+
			"have '%s', expecting '%s'", route.Ipr.DstAddr, vrfID, route.Ipr.OutIface, eOutIface)
		v.Report.LogErrAndAppendToNodeReport(nodeName, errString)
	}

	if route.IprMeta.OutgoingIfIdx != eOutgoingIfIdx {
		numErrs++
		rtMap[vrfID][route.Ipr.DstAddr] = routeInvalid
		errString := fmt.Sprintf("invalid route %s in VRF%d; bad outgoing swIndex - "+
			"have '%d', expecting '%d'", route.Ipr.DstAddr, vrfID, route.IprMeta.OutgoingIfIdx, eOutgoingIfIdx)
		v.Report.LogErrAndAppendToNodeReport(nodeName, errString)
	}

	if route.Ipr.ViaVRFID != eViaVrf {
		numErrs++
		rtMap[vrfID][route.Ipr.DstAddr] = routeInvalid
		errString := fmt.Sprintf("invalid route %s in VRF%d; bad viaVrfID - "+
			"have '%d', expecting '%d'", route.Ipr.DstAddr, vrfID, route.Ipr.ViaVRFID, eViaVrf)
		v.Report.LogErrAndAppendToNodeReport(nodeName, errString)
	}

	// eNextHop is empty if the next hop should not be validated
	if (eNextHopAddr != "") && (route.Ipr.NextHopAddr != eNextHopAddr) {
		numErrs++
		rtMap[vrfID][route.Ipr.DstAddr] = routeInvalid
		errString := fmt.Sprintf("invalid route %s in VRF%d; bad nextHop -"+
			"have '%s', expecting '%s", route.Ipr.DstAddr, vrfID, route.Ipr.NextHopAddr, eNextHopAddr)
		v.Report.LogErrAndAppendToNodeReport(nodeName, errString)
	}

	return numErrs
}

func separateIPandMask(ipAddress string) (string, string) {
	s := strings.Split(ipAddress, "/")
	if len(s) == 2 {
		return s[0], s[1]
	}
	return s[0], ""
}

func findInterface(name string, ifcs telemetrymodel.NodeInterfaces) (*telemetrymodel.NodeInterface, bool, error) {
	for _, ifc := range ifcs {
		match, err := regexp.Match(name, []byte(ifc.If.Name))
		if err != nil {
			return nil, false, err
		}
		if match {
			return &ifc, true, nil
		}
	}
	return nil, false, nil
}

func printS(errCnt int) string {
	if errCnt > 0 {
		return "s"
	}
	return ""
}

func printValidationMap(routeMap map[uint32]map[string]int) {
	for idx, vrf := range routeMap {
		fmt.Printf("VRF%d: routes %d\n", idx, len(vrf))
		for rte, sts := range vrf {
			if sts == routeNotValidated {
				fmt.Printf("x ")
			} else {
				fmt.Printf("  ")
			}

			fmt.Printf("{%s, %d}\n", rte, sts)
		}
		fmt.Println("")
	}
	fmt.Println("")

}
