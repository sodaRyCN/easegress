package api

import (
	"encoding/json"
	"fmt"
	"github.com/megaease/easegress/pkg/api"
	"github.com/megaease/easegress/pkg/logger"
	"github.com/megaease/easegress/pkg/object/meshcontroller/spec"
	"github.com/megaease/easegress/pkg/util/stringtool"
	"github.com/megaease/easegress/pkg/v"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"gopkg.in/yaml.v2"
	"io"
	"net/http"
	"path"
	"reflect"
)

const (
	// MeshServicePathV2 is the mesh service path.
	MeshServicePathV2 = "/v2/mesh/services/{serviceName}"

	// MeshServicePrefixV2 is mesh service prefix.
	MeshServicePrefixV2 = "/v2/mesh/services"
)

type vmServicesByOrder []*spec.VMService

func (s vmServicesByOrder) Less(i, j int) bool { return s[i].Name < s[j].Name }
func (s vmServicesByOrder) Len() int           { return len(s) }
func (s vmServicesByOrder) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

const apiV2GroupName = "mesh_admin_v2"

func (a *API) RegisterAPIsV2() {
	groupV2 := &api.Group{
		Group: apiV2GroupName,
		Entries: []*api.Entry{
			{Path: MeshServicePathV2, Method: "DELETE", Handler: a.deleteVMService},
			{Path: MeshServicePrefixV2, Method: "GET", Handler: a.listVMServices},
			{Path: MeshServicePrefixV2, Method: "POST", Handler: a.createVMService},
			{Path: MeshServicePathV2, Method: "GET", Handler: a.getVMService},
			{Path: MeshServicePathV2, Method: "PUT", Handler: a.updateVMService},

			// TODO: API to get instances of one service.

			{Path: MeshServiceInstancePrefix, Method: "GET", Handler: a.listServiceInstanceSpecs},
			{Path: MeshServiceInstancePath, Method: "GET", Handler: a.getServiceInstanceSpec},
			{Path: MeshServiceInstancePath, Method: "DELETE", Handler: a.offlineServiceInstance},
		},
	}

	api.RegisterAPIs(groupV2)
}

func (a *API) readAPIV2Spec(r *http.Request, spec interface{}) error {
	// TODO: Use default spec and validate it.

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("read body failed: %v", err)
	}

	err = json.Unmarshal(body, spec)
	if err != nil {
		return fmt.Errorf("unmarshal %s to spec failed: %v", string(body), err)
	}

	vr := v.Validate(spec)
	if !vr.Valid() {
		return fmt.Errorf("validate failed:\n%s", vr)
	}

	return nil
}

func (a *API) coverToServiceGeneric(kv *mvccpb.KeyValue) interface{} {
	vp := &spec.VMService{}
	err := yaml.Unmarshal(kv.Value, vp)
	if err == nil && vp.Name != "" {
		return vp
	}
	logger.Infof("convert spec %#v to vm service spec failed")
	p := &spec.Service{}
	err = yaml.Unmarshal(kv.Value, p)
	if err != nil || p.Name == "" {
		logger.Errorf("convert spec %#v to pb spec failed: %v", kv, err)
		return nil
	}
	return p
}

func (a *API) listVMServices(w http.ResponseWriter, r *http.Request) {
	kvs := a.service.ListRawServiceSpecs()

	//sort.Sort(vmServicesByOrder(specs))

	var apiSpecs []interface{}
	for _, value := range kvs {
		p := a.coverToServiceGeneric(value)
		if p == nil {
			continue
		}
		apiSpecs = append(apiSpecs, p)
	}

	buff, err := json.Marshal(apiSpecs)
	if err != nil {
		panic(fmt.Errorf("marshal %#value to json failed: %value", apiSpecs, err))
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(buff)
}

func (a *API) createVMService(w http.ResponseWriter, r *http.Request) {

	serviceSpec := &spec.VMService{}

	err := a.readAPIV2Spec(r, serviceSpec)
	if err != nil {
		api.HandleAPIError(w, r, http.StatusBadRequest, err)
		return
	}

	a.service.Lock()
	defer a.service.Unlock()

	oldSpec := a.service.GetVMServiceSpec(serviceSpec.Name)
	if oldSpec != nil {
		api.HandleAPIError(w, r, http.StatusConflict, fmt.Errorf("%s existed", serviceSpec.Name))
		return
	}

	tenantSpec := a.service.GetTenantSpec(serviceSpec.RegisterTenant)
	if tenantSpec == nil {
		api.HandleAPIError(w, r, http.StatusBadRequest,
			fmt.Errorf("tenant %s not found", serviceSpec.RegisterTenant))
		return
	}

	tenantSpec.Services = append(tenantSpec.Services, serviceSpec.Name)

	a.service.PutVMServiceSpec(serviceSpec)
	a.service.PutTenantSpec(tenantSpec)

	w.Header().Set("Location", path.Join(r.URL.Path, serviceSpec.Name))
	w.WriteHeader(http.StatusCreated)
}

func (a *API) getVMService(w http.ResponseWriter, r *http.Request) {
	serviceName, err := a.readServiceName(r)
	if err != nil {
		api.HandleAPIError(w, r, http.StatusBadRequest, err)
		return
	}

	_, rawKv := a.service.GetVMServiceSpecWithInfo(serviceName)
	if rawKv == nil {
		api.HandleAPIError(w, r, http.StatusNotFound, fmt.Errorf("%s not found", serviceName))
		return
	}
	serviceSpec := a.coverToServiceGeneric(rawKv)
	if serviceSpec != nil {
		panic(fmt.Errorf("convert spec %#v to pb failed: %v", rawKv, err))
	}

	buff, err := json.Marshal(serviceSpec)
	if err != nil {
		panic(fmt.Errorf("marshal %#v to json failed: %v", serviceSpec, err))
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(buff)
}

func (a *API) updateVMService(w http.ResponseWriter, r *http.Request) {

	serviceSpec := &spec.VMService{}

	serviceName, err := a.readServiceName(r)
	if err != nil {
		api.HandleAPIError(w, r, http.StatusBadRequest, err)
		return
	}
	err = a.readAPIV2Spec(r, serviceSpec)
	if err != nil {
		api.HandleAPIError(w, r, http.StatusBadRequest, err)
		return
	}
	if serviceName != serviceSpec.Name {
		api.HandleAPIError(w, r, http.StatusBadRequest,
			fmt.Errorf("name conflict: %s %s", serviceName, serviceSpec.Name))
		return
	}

	a.service.Lock()
	defer a.service.Unlock()

	oldSpec := a.service.GetVMServiceSpec(serviceName)
	if oldSpec == nil {
		api.HandleAPIError(w, r, http.StatusNotFound, fmt.Errorf("%s not found", serviceName))
		return
	}

	if serviceSpec.RegisterTenant != oldSpec.RegisterTenant {
		newTenantSpec := a.service.GetTenantSpec(serviceSpec.RegisterTenant)
		if newTenantSpec == nil {
			api.HandleAPIError(w, r, http.StatusBadRequest,
				fmt.Errorf("tenant %s not found", serviceSpec.RegisterTenant))
			return
		}
		newTenantSpec.Services = append(newTenantSpec.Services, serviceSpec.Name)

		oldTenantSpec := a.service.GetTenantSpec(oldSpec.RegisterTenant)
		if oldTenantSpec == nil {
			panic(fmt.Errorf("tenant %s not found", oldSpec.RegisterTenant))
		}
		oldTenantSpec.Services = stringtool.DeleteStrInSlice(oldTenantSpec.Services, serviceName)

		a.service.PutTenantSpec(newTenantSpec)
		a.service.PutTenantSpec(oldTenantSpec)
	}

	globalCanaryHeaders := a.service.GetGlobalCanaryHeaders()
	uniqueHeaders := serviceSpec.UniqueCanaryHeaders()
	oldUniqueHeaders := oldSpec.UniqueCanaryHeaders()

	if !reflect.DeepEqual(uniqueHeaders, oldUniqueHeaders) {
		if globalCanaryHeaders == nil {
			globalCanaryHeaders = &spec.GlobalCanaryHeaders{
				ServiceHeaders: map[string][]string{},
			}
		}
		globalCanaryHeaders.ServiceHeaders[serviceName] = uniqueHeaders
		a.service.PutGlobalCanaryHeaders(globalCanaryHeaders)
	}

	a.service.PutVMServiceSpec(serviceSpec)
}

func (a *API) deleteVMService(w http.ResponseWriter, r *http.Request) {
	serviceName, err := a.readServiceName(r)
	if err != nil {
		api.HandleAPIError(w, r, http.StatusBadRequest, err)
		return
	}

	a.service.Lock()
	defer a.service.Unlock()

	oldSpec := a.service.GetVMServiceSpec(serviceName)
	if oldSpec == nil {
		api.HandleAPIError(w, r, http.StatusNotFound, fmt.Errorf("%s not found", serviceName))
		return
	}

	tenantSpec := a.service.GetTenantSpec(oldSpec.RegisterTenant)
	if tenantSpec == nil {
		panic(fmt.Errorf("tenant %s not found", oldSpec.RegisterTenant))
	}

	tenantSpec.Services = stringtool.DeleteStrInSlice(tenantSpec.Services, serviceName)

	a.service.PutTenantSpec(tenantSpec)
	a.service.DeleteServiceSpec(serviceName)
}
