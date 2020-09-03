// Copyright 2020 Acnodal Inc.
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
package lbnodeagent

import (
	"net"

	"purelb.io/internal/acnodal"
	"purelb.io/internal/config"
	"purelb.io/internal/election"
	"purelb.io/internal/k8s"
	"purelb.io/internal/local"

	"github.com/go-kit/kit/log"
	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
)

type controller struct {
	logger     log.Logger
	myNode     string
	prometheus *prometheus.GaugeVec
	announcers []Announcer
	svcIP      map[string]net.IP // service name -> assigned IP
}

func NewController(l log.Logger, myNode string, prometheus *prometheus.GaugeVec) (*controller, error) {
	con := &controller{
		logger:     l,
		myNode:     myNode,
		prometheus: prometheus,
		announcers:  []Announcer{
			local.NewAnnouncer(l, myNode),
			acnodal.NewAnnouncer(l, myNode),
		},
		svcIP:      map[string]net.IP{},
	}

	return con, nil
}

func (c *controller) ServiceChanged(l log.Logger, name string, svc *v1.Service, _ *v1.Endpoints) k8s.SyncState {

	l.Log("event", "startUpdate", "msg", "start of service update", "service", name)
	defer l.Log("event", "endUpdate", "msg", "end of service update", "service", name)

	if svc == nil {
		return c.deleteBalancer(l, name, "serviceDeleted")
	}

	if len(svc.Status.LoadBalancer.Ingress) != 1 {
		return c.deleteBalancer(l, name, "noIPAllocated")
	}

	lbIP := net.ParseIP(svc.Status.LoadBalancer.Ingress[0].IP)
	if lbIP == nil {
		l.Log("op", "setBalancer", "error", "invalid LoadBalancer IP", svc.Status.LoadBalancer.Ingress[0].IP)
		return c.deleteBalancer(l, name, "invalidIP")
	}

	l = log.With(l, "ip", lbIP)

	if err := c.announcers[0].SetBalancer(name, lbIP, ""); err != nil {
		l.Log("op", "setBalancer", "error", err, "msg", "failed to announce service")
		return k8s.SyncStateError
	}

	c.prometheus.With(prometheus.Labels{
		"service": name,
		"node":    c.myNode,
		"ip":      lbIP.String(),
	}).Set(1)
	l.Log("event", "serviceAnnounced", "node", c.myNode, "msg", "service has IP, announcing")

	c.svcIP[name] = lbIP

	return k8s.SyncStateSuccess
}

func (c *controller) deleteBalancer(l log.Logger, name, reason string) k8s.SyncState {
	retval := k8s.SyncStateSuccess

	for _, announcer := range c.announcers {
		if err := announcer.DeleteBalancer(name, reason); err != nil {
			l.Log("op", "deleteBalancer", "error", err, "msg", "failed to clear balancer state")
			retval = k8s.SyncStateError
		}
	}

	c.prometheus.Delete(prometheus.Labels{
		"service": name,
		"node":    c.myNode,
		"ip":      c.svcIP[name].String(),
	})
	delete(c.svcIP, name)

	l.Log("event", "serviceWithdrawn", "ip", c.svcIP[name], "reason", reason, "msg", "withdrawing service announcement")

	return retval
}

func (c *controller) SetConfig(l log.Logger, cfg *config.Config) k8s.SyncState {
	l.Log("event", "startUpdate", "msg", "start of config update")
	defer l.Log("event", "endUpdate", "msg", "end of config update")

	retval := k8s.SyncStateReprocessAll

	for _, announcer := range c.announcers {
		if err := announcer.SetConfig(cfg); err != nil {
			l.Log("op", "setConfig", "error", err)
			retval = k8s.SyncStateError
		}
	}

	return retval
}

func (c *controller) SetNode(l log.Logger, node *v1.Node) k8s.SyncState {
	retval := k8s.SyncStateSuccess
	for _, announcer := range c.announcers {
		if err := announcer.SetNode(node); err != nil {
			l.Log("op", "setNode", "error", err)
			retval = k8s.SyncStateError
		}
	}
	return retval
}

func (c *controller) SetElection(election *election.Election) {
	for _, announcer := range c.announcers {
		announcer.SetElection(election)
	}
}