// Copyright 2019 Istio Authors
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

package kube

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/go-multierror"

	"istio.io/istio/pilot/pkg/model"
	appEcho "istio.io/istio/pkg/test/echo/client"
	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/echo/common"
	kubeEnv "istio.io/istio/pkg/test/framework/components/environment/kube"
	"istio.io/istio/pkg/test/framework/resource"
	"istio.io/istio/pkg/test/kube"

	kubeCore "k8s.io/api/core/v1"
)

const (
	tcpHealthPort     = 3333
	httpReadinessPort = 8080
	defaultDomain     = "svc.cluster.local"
)

var (
	_ echo.Instance = &instance{}
	_ io.Closer     = &instance{}
)

type instance struct {
	id        resource.ID
	cfg       echo.Config
	clusterIP string
	env       *kubeEnv.Environment
	workloads []*workload
	grpcPort  uint16
	mutex     sync.Mutex
}

func New(ctx resource.Context, cfg echo.Config) (out echo.Instance, err error) {
	// Fill in defaults for any missing values.
	if err = common.FillInDefaults(ctx, defaultDomain, &cfg); err != nil {
		return nil, err
	}

	// Validate the configuration.
	if cfg.Galley == nil {
		// Galley is not actually required currently, but it will be once Pilot gets
		// all resources from Galley. Requiring now for forward-compatibility.
		return nil, errors.New("galley must be provided")
	}

	env := ctx.Environment().(*kubeEnv.Environment)
	c := &instance{
		env: env,
		cfg: cfg,
	}
	c.id = ctx.TrackResource(c)

	// Save the GRPC port.
	grpcPort := common.GetGRPCPort(&cfg)
	if grpcPort == nil {
		return nil, errors.New("unable fo find GRPC command port")
	}
	c.grpcPort = uint16(grpcPort.InstancePort)

	// Generate the deployment YAML.
	generatedYAML, err := generateYAML(cfg)
	if err != nil {
		return nil, err
	}

	// Deploy the YAML.
	if err = env.ApplyContents(cfg.Namespace.Name(), generatedYAML); err != nil {
		return nil, err
	}

	// Now retrieve the service information to find the ClusterIP
	s, err := env.GetService(cfg.Namespace.Name(), cfg.Service)
	if err != nil {
		return nil, err
	}

	c.clusterIP = s.Spec.ClusterIP
	switch c.clusterIP {
	case kubeCore.ClusterIPNone, "":
		if !cfg.Headless {
			return nil, fmt.Errorf("invalid ClusterIP %s for non-headless service %s/%s",
				c.clusterIP,
				c.cfg.Namespace.Name(),
				c.cfg.Service)
		}
		c.clusterIP = ""
	}

	return c, nil
}

// getContainerPorts converts the ports to a port list of container ports.
// Adds ports for health/readiness if necessary.
func getContainerPorts(ports []echo.Port) model.PortList {
	containerPorts := make(model.PortList, 0, len(ports))
	var healthPort *model.Port
	var readyPort *model.Port
	for _, p := range ports {
		// Add the port to the set of application ports.
		cport := &model.Port{
			Name:     p.Name,
			Protocol: p.Protocol,
			Port:     p.InstancePort,
		}
		containerPorts = append(containerPorts, cport)

		switch p.Protocol {
		case model.ProtocolGRPC:
			continue
		case model.ProtocolHTTP:
			if p.InstancePort == httpReadinessPort {
				readyPort = cport
			}
		default:
			if p.InstancePort == tcpHealthPort {
				healthPort = cport
			}
		}
	}

	// If we haven't added the readiness/health ports, do so now.
	if readyPort == nil {
		containerPorts = append(containerPorts, &model.Port{
			Name:     "http-readiness-port",
			Protocol: model.ProtocolHTTP,
			Port:     httpReadinessPort,
		})
	}
	if healthPort == nil {
		containerPorts = append(containerPorts, &model.Port{
			Name:     "tcp-health-port",
			Protocol: model.ProtocolHTTP,
			Port:     tcpHealthPort,
		})
	}
	return containerPorts
}

func (c *instance) ID() resource.ID {
	return c.id
}

func (c *instance) Address() string {
	return c.clusterIP
}

func (c *instance) Workloads() ([]echo.Workload, error) {
	if err := c.WaitUntilReady(); err != nil {
		return nil, err
	}

	out := make([]echo.Workload, 0, len(c.workloads))
	for _, w := range c.workloads {
		out = append(out, w)
	}
	return out, nil
}

func (c *instance) WorkloadsOrFail(t testing.TB) []echo.Workload {
	out, err := c.Workloads()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func initAllWorkloads(accessor *kube.Accessor, instances []echo.Instance) error {
	needInit := getUninitializedInstances(instances)
	if len(needInit) == 0 {
		// Everything is already initialized.
		return nil
	}

	instanceEndpoints := make([]*kubeCore.Endpoints, len(needInit))
	aggregateErrMux := &sync.Mutex{}
	var aggregateErr error
	wg := sync.WaitGroup{}

	for i, inst := range instances {
		wg.Add(1)

		instanceIndex := i
		serviceName := inst.Config().Service
		serviceNamespace := inst.Config().Namespace.Name()

		// Run the waits in parallel.
		go func() {
			defer wg.Done()

			// Wait until all the endpoints are ready for this service
			_, endpoints, err := accessor.WaitUntilServiceEndpointsAreReady(serviceNamespace, serviceName)
			if err != nil {
				aggregateErrMux.Lock()
				aggregateErr = multierror.Append(aggregateErr, err)
				aggregateErrMux.Unlock()
				return
			}
			instanceEndpoints[instanceIndex] = endpoints
		}()
	}

	wg.Wait()

	if aggregateErr != nil {
		return aggregateErr
	}

	// Initialize the workloads for each instance.
	for i, inst := range needInit {
		if err := inst.initWorkloads(instanceEndpoints[i]); err != nil {
			return err
		}
	}
	return nil
}

func (c *instance) isInitialized() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.workloads != nil
}

func getUninitializedInstances(instances []echo.Instance) []*instance {
	uninitialized := make([]*instance, 0, len(instances))
	for _, i := range instances {
		inst := i.(*instance)
		if !inst.isInitialized() {
			uninitialized = append(uninitialized, inst)
		}
	}
	return uninitialized
}

func (c *instance) WaitUntilReady(outboundInstances ...echo.Instance) error {

	// Initialize the workloads for all instances.
	if err := initAllWorkloads(c.env.Accessor, append([]echo.Instance{c}, outboundInstances...)); err != nil {
		return err
	}

	// Wait for the outbound config to be received by each workload from Pilot.
	for _, w := range c.workloads {
		if w.sidecar != nil {
			if err := w.sidecar.WaitForConfig(common.OutboundConfigAcceptFunc(outboundInstances...)); err != nil {
				return err
			}
		}
	}

	return nil
}

func (c *instance) WaitUntilReadyOrFail(t testing.TB, outboundInstances ...echo.Instance) {
	if err := c.WaitUntilReady(outboundInstances...); err != nil {
		t.Fatal(err)
	}
}

func (c *instance) initWorkloads(endpoints *kubeCore.Endpoints) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.workloads != nil {
		// Already ready.
		return nil
	}

	workloads := make([]*workload, 0)
	for _, subset := range endpoints.Subsets {
		for _, addr := range subset.Addresses {
			workload, err := newWorkload(addr, c.cfg.Sidecar, c.grpcPort, c.env.Accessor)
			if err != nil {
				return err
			}
			workloads = append(workloads, workload)
		}
	}

	if len(workloads) == 0 {
		return fmt.Errorf("no pods found for service %s/%s/%s", c.cfg.Namespace.Name(), c.cfg.Service, c.cfg.Version)
	}

	c.workloads = workloads
	return nil
}

func (c *instance) Close() (err error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	for _, w := range c.workloads {
		err = multierror.Append(err, w.Close()).ErrorOrNil()
	}
	c.workloads = nil
	return
}

func (c *instance) Config() echo.Config {
	return c.cfg
}

func (c *instance) Call(opts echo.CallOptions) (appEcho.ParsedResponses, error) {
	// If we haven't already initialized the client, do so now.
	if err := c.WaitUntilReady(); err != nil {
		return nil, err
	}

	out, err := common.CallEcho(c.workloads[0].Instance, &opts, common.IdentityOutboundPortSelector)
	if err != nil {
		if opts.Port != nil {
			err = fmt.Errorf("failed calling %s->'%s://%s:%d/%s': %v",
				c.Config().Service,
				strings.ToLower(string(opts.Port.Protocol)),
				opts.Target.Config().Service,
				opts.Port.ServicePort,
				opts.Path,
				err)
		}
		return nil, err
	}
	return out, nil
}

func (c *instance) CallOrFail(t testing.TB, opts echo.CallOptions) appEcho.ParsedResponses {
	r, err := c.Call(opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
