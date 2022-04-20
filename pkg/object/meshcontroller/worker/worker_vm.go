/*
 * Copyright (c) 2017, MegaEase
 * All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package worker

import (
	"fmt"
	"gopkg.in/yaml.v2"
	"os"
	"runtime/debug"
	"time"

	"github.com/megaease/easegress/pkg/logger"
	"github.com/megaease/easegress/pkg/object/meshcontroller/informer"
	"github.com/megaease/easegress/pkg/object/meshcontroller/label"
	"github.com/megaease/easegress/pkg/object/meshcontroller/layout"
	"github.com/megaease/easegress/pkg/object/meshcontroller/registrycenter"
	"github.com/megaease/easegress/pkg/object/meshcontroller/service"
	"github.com/megaease/easegress/pkg/object/meshcontroller/spec"
	"github.com/megaease/easegress/pkg/object/meshcontroller/storage"
	"github.com/megaease/easegress/pkg/supervisor"
)

type VMWorker struct {
	*Worker
	registryServer *registrycenter.VMServer
	ingressServer  *VMIngressServer
	egressServer   *VMEgressServer
}

const (
	vmWorkerName = "vm-worker"
)

// NewVMWorker creates a mesh vm level rawworker.
func NewVMWorker(superSpec *supervisor.Spec) RawWorker {
	super := superSpec.Super()
	_spec := superSpec.ObjectSpec().(*spec.Admin)
	serviceName := vmWorkerName
	serviceLabels := decodeLabels(super.Options().Labels[label.KeyServiceLabels])

	instanceID := os.Getenv(spec.PodEnvHostname)
	ip := os.Getenv(spec.PodEnvApplicationIP)
	store := storage.New(superSpec.Name(), super.Cluster())
	_service := service.New(superSpec)

	inf := informer.NewInformer(store, serviceName)
	registryCenterServer := registrycenter.NewVMRegistryCenterServer(_spec.RegistryType,
		superSpec.Name(), serviceName, ip,
		instanceID, serviceLabels, _service, inf)
	ingressServer := NewVMIngressServer(superSpec, super, serviceName, _service)
	egressServer := NewVMEgressServer(superSpec, super, serviceName, instanceID, _service)

	apiServer := newAPIServer(_spec.APIPort)

	worker := &VMWorker{
		Worker: &Worker{
			super:     super,
			superSpec: superSpec,
			spec:      _spec,

			serviceName:   serviceName,
			instanceID:    instanceID, // instanceID will be the pod ID valued by HOSTNAME env.
			serviceLabels: serviceLabels,
			store:         store,
			service:       _service,
			informer:      inf,
			apiServer:     apiServer,

			done: make(chan struct{}),
		},
		registryServer: registryCenterServer,
		ingressServer:  ingressServer,
		egressServer:   egressServer,
	}

	worker.runAPIServer()

	go worker.run()

	logger.Infof("vm vm worker instance %s controller plane is on %s, data plane is on %s:%d", instanceID, super.Options().APIAddr, ip, _spec.APIPort)

	return worker
}

func (worker *VMWorker) validate() error {
	var err error
	worker.heartbeatInterval, err = time.ParseDuration(worker.spec.HeartbeatInterval)
	if err != nil {
		logger.Errorf("BUG: parse heartbeat interval: %s failed: %v",
			worker.spec.HeartbeatInterval, err)
		return err
	}

	if len(worker.instanceID) == 0 {
		errMsg := "empty vm rawworker instance id "
		logger.Errorf(errMsg)
		return fmt.Errorf(errMsg)
	}

	logger.Infof("sidecar works for service: %s", worker.serviceName)
	return nil
}

func (worker *VMWorker) run() {
	if err := worker.validate(); err != nil {
		return
	}
	startUpRoutine := func() bool {
		defer func() {
			if err := recover(); err != nil {
				logger.Errorf("%s: recover from: %v, stack trace:\n%s\n",
					worker.superSpec.Name(), err, debug.Stack())
			}
		}()

		serviceSpec, _ := worker.service.GetServiceSpecWithInfo(worker.serviceName)
		if serviceSpec == nil || !serviceSpec.Runnable() {
			return false
		}

		//err := rawworker.initTrafficGate()
		//if err != nil {
		//	logger.Errorf("init traffic gate failed: %v", err)
		//}
		err := worker.egressServer.InitEgress(serviceSpec)
		if err != nil {
			logger.Errorf("init egress fail, spec is %v", serviceSpec)
			return false
		}

		worker.registryServer.RegisterItself(uint32(worker.spec.APIPort))

		//err = rawworker.observabilityManager.UpdateService(serviceSpec, info.Version)
		//if err != nil {
		//	logger.Errorf("update service %s failed: %v", serviceSpec.Name, err)
		//}

		return true
	}

	if runnable := startUpRoutine(); !runnable {
		logger.Errorf("service: %s is not runnable, check the service spec or ignore if mock is enable", worker.superSpec.Name())
		return
	}
	go worker.heartbeat()
	//go rawworker.pushSpecToJavaAgent()
}

func (worker *VMWorker) heartbeat() {
	//informJavaAgentReady, trafficGateReady := false, false

	//routine := func() {
	//	defer func() {
	//		if err := recover(); err != nil {
	//			logger.Errorf("%s: recover from: %v, stack trace:\n%s\n",
	//				worker.superSpec.Name(), err, debug.Stack())
	//		}
	//	}()
	//
	//	if !trafficGateReady {
	//		err := worker.initTrafficGate()
	//		if err != nil {
	//			logger.Errorf("init traffic gate failed: %v", err)
	//		} else {
	//			trafficGateReady = true
	//		}
	//	}
	//
	//	if worker.registryServer.Registered() {
	//		if !informJavaAgentReady {
	//			err := worker.informJavaAgent()
	//			if err != nil {
	//				logger.Errorf(err.Error())
	//			} else {
	//				informJavaAgentReady = true
	//			}
	//		}
	//
	//		err := worker.updateHeartbeat()
	//		if err != nil {
	//			logger.Errorf("update heartbeat failed: %v", err)
	//		}
	//	}
	//}

	for {
		select {
		case <-worker.done:
			return
		case <-time.After(worker.heartbeatInterval):
			worker.updateHeartbeat(worker.serviceName, worker.instanceID)
		}
	}
}

func (worker *VMWorker) pushSpecToJavaAgent() {
	//routine := func() {
	//	defer func() {
	//		if err := recover(); err != nil {
	//			logger.Errorf("%s: recover from: %v, stack trace:\n%s\n",
	//				rawworker.superSpec.Name(), err, debug.Stack())
	//		}
	//	}()
	//
	//	serviceSpec, info := rawworker.service.GetServiceSpecWithInfo(rawworker.serviceName)
	//	err := rawworker.observabilityManager.UpdateService(serviceSpec, info.Version)
	//	if err != nil {
	//		logger.Errorf("update service %s failed: %v", serviceSpec.Name, err)
	//	}
	//
	//	globalCanaryHeaders, info := rawworker.service.GetGlobalCanaryHeadersWithInfo()
	//	if globalCanaryHeaders != nil {
	//		err := rawworker.observabilityManager.UpdateCanary(globalCanaryHeaders, info.Version)
	//		if err != nil {
	//			logger.Errorf("update canary failed: %v", err)
	//		}
	//	}
	//}
	//
	//for {
	//	select {
	//	case <-rawworker.done:
	//		return
	//	case <-time.After(1 * time.Minute):
	//		routine()
	//	}
	//}
}

// only init httpserver and pipeline with object base properties like object name,so on
func (worker *VMWorker) initTrafficGate() error {
	serviceSpec := worker.service.GetServiceSpec(worker.serviceName)
	if serviceSpec == nil {
		logger.Errorf("serviceSpec %s not found", worker.serviceName)
		return spec.ErrServiceNotFound
	}

	if err := worker.ingressServer.InitIngress(serviceSpec, worker.applicationPort); err != nil {
		return fmt.Errorf("create ingress for serviceSpec: %s failed: %v", worker.serviceName, err)
	}

	if err := worker.egressServer.InitEgress(serviceSpec); err != nil {
		return fmt.Errorf("create egress for serviceSpec: %s failed: %v", worker.serviceName, err)
	}

	return nil
}

func (worker *VMWorker) updateHeartbeat(name, instanceID string) error {
	//resp, err := http.Get(worker.aliveProbe)
	//if err != nil {
	//	return fmt.Errorf("probe: %s check service: %s instanceID: %s heartbeat failed: %v",
	//		worker.aliveProbe, worker.serviceName, worker.instanceID, err)
	//}
	//defer resp.Body.Close()
	//
	//if resp.StatusCode != http.StatusOK {
	//	return fmt.Errorf("probe: %s check service: %s instanceID: %s heartbeat failed status code is %d",
	//		worker.aliveProbe, worker.serviceName, worker.instanceID, resp.StatusCode)
	//}

	value, err := worker.store.Get(layout.ServiceInstanceStatusKey(name, instanceID))
	if err != nil {
		return fmt.Errorf("get service: %s instance: %s status failed: %v", worker.serviceName, worker.instanceID, err)
	}

	status := &spec.ServiceInstanceStatus{
		ServiceName: name,
		InstanceID:  instanceID,
	}
	if value != nil {
		err := yaml.Unmarshal([]byte(*value), status)
		if err != nil {
			logger.Errorf("BUG: unmarshal %s to yaml failed: %v", *value, err)

			// NOTE: This is a little strict, maybe we could use the brand new status to update.
			return err
		}
	}

	status.LastHeartbeatTime = time.Now().Format(time.RFC3339)
	buff, err := yaml.Marshal(status)
	if err != nil {
		logger.Errorf("BUG: marshal %#v to yaml failed: %v", status, err)
		return err
	}

	return worker.store.Put(layout.ServiceInstanceStatusKey(name, instanceID), string(buff))
}

func (worker *VMWorker) informJavaAgent() error {
	return nil
	//handleServiceSpec := func(event informer.Event, service *spec.Service) bool {
	//	switch event.EventType {
	//	case informer.EventDelete:
	//		return false
	//	case informer.EventUpdate:
	//		if err := rawworker.observabilityManager.UpdateService(service, event.RawKV.Version); err != nil {
	//			logger.Errorf("update service %s failed: %v", service.Name, err)
	//		}
	//	}
	//
	//	return true
	//}
	//
	//err := rawworker.informer.OnPartOfServiceSpec(rawworker.serviceName, handleServiceSpec)
	//if err != nil && err != informer.ErrAlreadyWatched {
	//	return fmt.Errorf("on informer for observability failed: %v", err)
	//}
	//
	//return nil
}
