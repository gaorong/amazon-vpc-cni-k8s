// Copyright 2017 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package ipamd

import (
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/pkg/errors"

	pb "github.com/aws/amazon-vpc-cni-k8s/rpc"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	log "github.com/cihub/seelog"

	"github.com/aws/amazon-vpc-cni-k8s/ipamd/datastore"
	"github.com/aws/amazon-vpc-cni-k8s/pkg/k8sapi"
)

const (
	ipamdgRPCaddress = "127.0.0.1:50051"
)

// server controls RPC service responses.
type server struct {
	ipamContext *IPAMContext
}

// AddNetwork processes CNI add network request and return an IP address for container
func (s *server) AddNetwork(ctx context.Context, in *pb.AddNetworkRequest) (*pb.AddNetworkReply, error) {
	log.Infof("Received AddNetwork for NS %s, Pod %s, NameSpace %s, Container %s, ifname %s",
		in.Netns, in.K8S_POD_NAME, in.K8S_POD_NAMESPACE, in.K8S_POD_INFRA_CONTAINER_ID, in.IfName)

	addr, deviceNumber, err := s.ipamContext.dataStore.AssignPodIPv4Address(&k8sapi.K8SPodInfo{
		Name:      in.K8S_POD_NAME,
		Namespace: in.K8S_POD_NAMESPACE,
		Container: in.K8S_POD_INFRA_CONTAINER_ID})

	var pbVPCcidrs []string
	for _, cidr := range s.ipamContext.awsClient.GetVPCIPv4CIDRs() {
		log.Debugf("VPC CIDR %s", *cidr)
		pbVPCcidrs = append(pbVPCcidrs, *cidr)
	}

	useExternalSNAT := s.ipamContext.networkClient.UseExternalSNAT()
	if !useExternalSNAT {
		for _, cidr := range s.ipamContext.networkClient.GetExcludeSNATCIDRs() {
			log.Debugf("CIDR SNAT Exclusion %s", cidr)
			pbVPCcidrs = append(pbVPCcidrs, cidr)
		}
	}

	resp := pb.AddNetworkReply{
		Success:         err == nil,
		IPv4Addr:        addr,
		IPv4Subnet:      "",
		DeviceNumber:    int32(deviceNumber),
		UseExternalSNAT: useExternalSNAT,
		VPCcidrs:        pbVPCcidrs,
	}

	log.Infof("Send AddNetworkReply: IPv4Addr %s, DeviceNumber: %d, err: %v", addr, deviceNumber, err)
	addIPCnt.Inc()
	return &resp, nil
}

func (s *server) DelNetwork(ctx context.Context, in *pb.DelNetworkRequest) (*pb.DelNetworkReply, error) {
	log.Infof("Received DelNetwork for IP %s, Pod %s, Namespace %s, Container %s",
		in.IPv4Addr, in.K8S_POD_NAME, in.K8S_POD_NAMESPACE, in.K8S_POD_INFRA_CONTAINER_ID)
	delIPCnt.With(prometheus.Labels{"reason": in.Reason}).Inc()

	ip, deviceNumber, err := s.ipamContext.dataStore.UnassignPodIPv4Address(&k8sapi.K8SPodInfo{
		Name:      in.K8S_POD_NAME,
		Namespace: in.K8S_POD_NAMESPACE,
		Container: in.K8S_POD_INFRA_CONTAINER_ID})

	if err != nil && err == datastore.ErrUnknownPod {
		// If L-IPAMD restarts, the pod's IP address are assigned by only pod's name and namespace due to kubelet's introspection.
		ip, deviceNumber, err = s.ipamContext.dataStore.UnassignPodIPv4Address(&k8sapi.K8SPodInfo{
			Name:      in.K8S_POD_NAME,
			Namespace: in.K8S_POD_NAMESPACE})
	}
	log.Infof("Send DelNetworkReply: IPv4Addr %s, DeviceNumber: %d, err: %v", ip, deviceNumber, err)

	// Plugins should generally complete a DEL action without error even if some resources are missing. For example,
	// an IPAM plugin should generally release an IP allocation and return success even if the container network
	// namespace no longer exists, unless that network namespace is critical for IPAM management
	success := true
	if err != nil && err != datastore.ErrUnknownPod {
		success = false
	}
	return &pb.DelNetworkReply{Success: success, IPv4Addr: ip, DeviceNumber: int32(deviceNumber)}, nil
}

// RunRPCHandler handles request from gRPC
func (c *IPAMContext) RunRPCHandler() error {
	log.Info("Serving RPC Handler on ", ipamdgRPCaddress)

	lis, err := net.Listen("tcp", ipamdgRPCaddress)
	if err != nil {
		log.Errorf("Failed to listen gRPC port: %v", err)
		return errors.Wrap(err, "ipamd: failed to listen to gRPC port")
	}
	s := grpc.NewServer()
	pb.RegisterCNIBackendServer(s, &server{ipamContext: c})
	hs := health.NewServer()
	// TODO: Implement watch once the status is check is handled correctly.
	hs.SetServingStatus("grpc.health.v1.aws-node", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(s, hs)
	// Register reflection service on gRPC server.
	reflection.Register(s)
	// Add shutdown hook
	go c.shutdownListener(s)
	if err := s.Serve(lis); err != nil {
		log.Errorf("Failed to start server on gRPC port: %v", err)
		return errors.Wrap(err, "ipamd: failed to start server on gPRC port")
	}
	return nil
}

// shutdownListener - Listen to signals and set ipamd to be in status "terminating"
func (c *IPAMContext) shutdownListener(s *grpc.Server) {
	log.Info("Setting up shutdown hook.")
	sig := make(chan os.Signal, 1)

	// Interrupt signal sent from terminal
	signal.Notify(sig, syscall.SIGINT)
	// Terminate signal sent from Kubernetes
	signal.Notify(sig, syscall.SIGTERM)

	<-sig
	log.Info("Received shutdown signal, setting 'terminating' to true")
	// We received an interrupt signal, shut down.
	c.setTerminating()
}
