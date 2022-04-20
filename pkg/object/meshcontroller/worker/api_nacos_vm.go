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
	"encoding/json"
	"fmt"
	"github.com/go-chi/chi/v5"
	"github.com/megaease/easegress/pkg/object/meshcontroller/spec"
	"github.com/megaease/easegress/pkg/v"
	"gopkg.in/yaml.v2"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/megaease/easegress/pkg/api"
	"github.com/megaease/easegress/pkg/logger"
	"github.com/megaease/easegress/pkg/object/meshcontroller/registrycenter"
)

func (worker *VMWorker) vmNacosAPIs() []*apiEntry {
	APIs := []*apiEntry{
		{
			Path:    meshNacosPrefix + "/ns/instance/list/{serviceName}",
			Method:  "GET",
			Handler: worker.nacosInstanceList,
		},
		{
			Path:    meshNacosPrefix + "/ns/instance",
			Method:  "POST",
			Handler: worker.nacosRegister,
		},
		{
			Path:    meshNacosPrefix + "/ns/instance",
			Method:  "DELETE",
			Handler: worker.emptyHandler,
		},
		{
			Path:    meshNacosPrefix + "/ns/instance/beat",
			Method:  "PUT",
			Handler: worker.emptyHandler,
		},
		{
			Path:    meshNacosPrefix + "/ns/instance",
			Method:  "PUT",
			Handler: worker.emptyHandler,
		},
		{
			Path:    meshNacosPrefix + "/ns/instance/{serviceName}/{instanceID}",
			Method:  "GET",
			Handler: worker.nacosInstance,
		},
		{
			Path:    meshNacosPrefix + "/ns/service/list",
			Method:  "GET",
			Handler: worker.nacosServiceList,
		},
		{
			Path:    meshNacosPrefix + "/ns/service",
			Method:  "GET",
			Handler: worker.nacosService,
		},
		{
			Path:    meshNacosPrefix + "/ns/instance/all/status",
			Method:  "GET",
			Handler: worker.listStatus,
		},
		{
			Path:    meshNacosPrefix + "/ns/instance/all/specs",
			Method:  "GET",
			Handler: worker.listSpecs,
		},
	}

	return APIs
}

func (worker *VMWorker) convertPBToSpec(rSpec *spec.RegistryInstance, vSpec *spec.VMService, sSpec *spec.StorageInstance) {
	sSpec.ServiceLabels = rSpec.ServiceLabels
	sSpec.Port = vSpec.Port
	sSpec.RegistryName = vSpec.RegistryName
	sSpec.RegisterTenant = vSpec.RegisterTenant
	sSpec.InstanceID = rSpec.InstanceID
	sSpec.Protocol = vSpec.Protocol
	sSpec.Name = rSpec.Name
	sSpec.IP = rSpec.IP
	sSpec.Status = spec.ServiceStatusUp
	sSpec.RegistryTime = time.Now().Format(time.RFC3339)
	sSpec.ProxyIP = os.Getenv(spec.PodEnvApplicationIP)
	sSpec.Sidecar = &spec.Sidecar{
		DiscoveryType:   spec.RegistryTypeNacos,
		Address:         worker.registryServer.IP,
		IngressPort:     vSpec.Sidecar.IngressPort,
		IngressProtocol: "",
		EgressPort:      0,
		EgressProtocol:  "",
	}
}

func (worker *VMWorker) readAPISpec(r *http.Request, pbSpec *spec.RegistryInstance, spec *spec.StorageInstance, serviceSpec *spec.VMService) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("read body failed: %v", err)
	}

	err = json.Unmarshal(body, pbSpec)
	if err != nil {
		return fmt.Errorf("unmarshal %s to pb spec %#v failed: %v", string(body), pbSpec, err)
	}

	if pbSpec.Name == "" {
		return fmt.Errorf("registry instance's service name %s not exist", pbSpec.Name)
	}
	_, kv := worker.service.GetVMServiceSpecWithInfo(pbSpec.Name)
	err = yaml.Unmarshal(kv.Value, serviceSpec)
	if err != nil {
		return fmt.Errorf("registry instance's service name %s not registried", pbSpec.Name)
	}

	worker.convertPBToSpec(pbSpec, serviceSpec, spec)

	vr := v.Validate(spec)
	if !vr.Valid() {
		return fmt.Errorf("validate failed:\n%s", vr)
	}

	return nil
}

func (worker *VMWorker) listStatus(w http.ResponseWriter, r *http.Request) {
	specs := worker.service.ListAllServiceInstanceStatuses()
	buff, err := json.Marshal(specs)
	if err != nil {
		logger.Errorf("json marshal nacosService: %#v err: %v", specs, err)
		api.HandleAPIError(w, r, http.StatusInternalServerError, err)
		return
	}

	w.Header().Set("Content-Type", registrycenter.ContentTypeJSON)
	w.Write(buff)
}

func (worker *VMWorker) listSpecs(w http.ResponseWriter, r *http.Request) {
	specs := worker.service.ListAllServiceInstanceSpecs()
	buff, err := json.Marshal(specs)
	if err != nil {
		logger.Errorf("json marshal nacosService: %#v err: %v", specs, err)
		api.HandleAPIError(w, r, http.StatusInternalServerError, err)
		return
	}

	w.Header().Set("Content-Type", registrycenter.ContentTypeJSON)
	w.Write(buff)
}

func (worker *VMWorker) nacosRegister(w http.ResponseWriter, r *http.Request) {
	registryInstance := &spec.RegistryInstance{}
	storageInstance := &spec.StorageInstance{}
	vmServiceSpec := &spec.VMService{}

	err := worker.readAPISpec(r, registryInstance, storageInstance, vmServiceSpec)
	if err != nil {
		api.HandleAPIError(w, r, http.StatusBadRequest, err)
		return
	}

	worker.service.Lock()
	defer worker.service.Unlock()

	oldSpec := worker.service.GetVMServiceSpec(storageInstance.Name)
	if oldSpec == nil {
		err := fmt.Errorf("registry to unknown service: %s", worker.serviceName)
		api.HandleAPIError(w, r, http.StatusBadRequest, err)
		return
	}

	if err = worker.ingressServer.RegistryIngress(storageInstance, vmServiceSpec); err != nil {
		logger.Errorf("registry ingress of %s fail", storageInstance.Name)
		return
	}

	worker.registryServer.Register(storageInstance, worker.ingressServer.Ready(storageInstance.Name), worker.egressServer.Ready())
	//worker.updateHeartbeat(serviceSpec.Name, serviceSpec.InstanceID)
}

func (worker *VMWorker) nacosInstanceList(w http.ResponseWriter, r *http.Request) {
	serviceName := chi.URLParam(r, "serviceName")
	if len(serviceName) == 0 {
		api.HandleAPIError(w, r, http.StatusBadRequest,
			fmt.Errorf("empty serviceName in url parameters"))
		return
	}

	specs := worker.service.ListServiceInstanceSpecs(serviceName)

	//nacosSvc := worker.registryServer.ToNacosService(serviceInfo)

	buff, err := json.Marshal(specs)
	if err != nil {
		logger.Errorf("json marshal nacosService: %#v err: %v", specs, err)
		api.HandleAPIError(w, r, http.StatusInternalServerError, err)
		return
	}

	w.Header().Set("Content-Type", registrycenter.ContentTypeJSON)
	w.Write(buff)
}

func (worker *VMWorker) nacosInstance(w http.ResponseWriter, r *http.Request) {
	serviceName := chi.URLParam(r, "serviceName")
	instanceID := chi.URLParam(r, "instanceID")
	if len(serviceName) == 0 || len(instanceID) == 0 {
		api.HandleAPIError(w, r, http.StatusBadRequest,
			fmt.Errorf("empty serviceName in url parameters"))
		return
	}

	instanceSpec := worker.service.GetServiceInstanceSpec(serviceName, instanceID)

	//nacosIns := worker.registryServer.ToNacosInstanceInfo(serviceInfo)

	buff, err := json.Marshal(instanceSpec)
	if err != nil {
		logger.Errorf("json marshal nacosInstance: %#v err: %v", instanceSpec, err)
		api.HandleAPIError(w, r, http.StatusInternalServerError, err)
		return
	}

	w.Header().Set("Content-Type", registrycenter.ContentTypeJSON)
	w.Write(buff)
}

func (worker *VMWorker) nacosServiceList(w http.ResponseWriter, r *http.Request) {
	var (
		err          error
		serviceInfos []*registrycenter.ServiceRegistryInfo
	)
	if serviceInfos, err = worker.registryServer.Discovery(); err != nil {
		logger.Errorf("discovery services err: %v ", err)
		api.HandleAPIError(w, r, http.StatusInternalServerError, err)
		return
	}
	serviceList := worker.registryServer.ToNacosServiceList(serviceInfos)

	buff, err := json.Marshal(serviceList)
	if err != nil {
		logger.Errorf("json marshal serviceList: %#v err: %v", serviceList, err)
		api.HandleAPIError(w, r, http.StatusInternalServerError, err)
		return
	}

	w.Header().Set("Content-Type", registrycenter.ContentTypeJSON)
	w.Write(buff)
}

func (worker *VMWorker) nacosService(w http.ResponseWriter, r *http.Request) {
	serviceName := chi.URLParam(r, "serviceName")
	if len(serviceName) == 0 {
		api.HandleAPIError(w, r, http.StatusBadRequest,
			fmt.Errorf("empty serviceName in url parameters"))
		return
	}

	var serviceInfo *spec.VMService
	var err error
	if serviceInfo, err = worker.registryServer.DiscoveryService(serviceName); err != nil {
		logger.Errorf("discovery service: %s, err: %v ", serviceName, err)
		api.HandleAPIError(w, r, http.StatusInternalServerError, err)
		return
	}

	//nacosSvcDetail := worker.registryServer.ToNacosServiceDetail(serviceInfo)

	buff, err := json.Marshal(serviceInfo)
	if err != nil {
		logger.Errorf("json marshal nacosSvcDetail: %#v err: %v", serviceInfo, err)
		api.HandleAPIError(w, r, http.StatusInternalServerError, err)
		return
	}

	w.Header().Set("Content-Type", registrycenter.ContentTypeJSON)
	w.Write(buff)
}
