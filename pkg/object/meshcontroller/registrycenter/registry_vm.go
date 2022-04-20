package registrycenter

import (
	"fmt"
	"github.com/go-chi/chi/v5"
	"github.com/megaease/easegress/pkg/logger"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/megaease/easegress/pkg/object/meshcontroller/informer"
	"github.com/megaease/easegress/pkg/object/meshcontroller/service"
	"github.com/megaease/easegress/pkg/object/meshcontroller/spec"
)

// VMServer handle all vm registry about logic
type (
	VMServer struct {
		*Server
		// ReadyFunc is a function to check Ingress/Egress ready to work
	}
)

// NewVMRegistryCenterServer creates an initialized registry center server.
func NewVMRegistryCenterServer(registryType string, registryName, serviceName string, IP string, instanceID string,
	serviceLabels map[string]string, service *service.Service, informer informer.Informer) *VMServer {
	return &VMServer{
		Server: &Server {
			RegistryType:  registryType,
			registryName:  registryName,
			serviceName:   serviceName,
			service:       service,
			registered:    false,
			mutex:         sync.RWMutex{},
			IP:            IP,
			instanceID:    instanceID,
			serviceLabels: serviceLabels,
			informer:      informer,

			done: make(chan struct{}),
		},
	}
}

// Register register service instance
func (rcs *VMServer) Register(service *spec.StorageInstance, ingressReady , egressReady ReadyFunc) {
	ins := &spec.ServiceInstanceSpec{
		RegistryName: service.RegistryName,
		ServiceName:  service.Name,
		InstanceID:   service.InstanceID,
		IP:           service.ProxyIP,
		Port:         uint32(service.Sidecar.IngressPort),
		Labels:       service.ServiceLabels,
	}

	go rcs.register(ins, ingressReady, egressReady)

	rcs.informer.OnPartOfServiceSpec(service.Name, rcs.onUpdateLocalInfo)
	rcs.informer.OnAllTrafficTargetSpecs(rcs.onAllTrafficTargetSpecs)
}


// RegisterItself register itself by specify port
func (rcs *VMServer) RegisterItself(port uint32) {
	ins := &spec.ServiceInstanceSpec{
		RegistryName: rcs.registryName,
		ServiceName:  rcs.serviceName,
		InstanceID:   rcs.instanceID,
		IP:           rcs.IP,
		Port:         port,
		Labels:       rcs.serviceLabels,
	}

	iReady := func() bool {return true}
	eReady := func() bool {return true}
	go rcs.register(ins, iReady, eReady)

	rcs.informer.OnPartOfServiceSpec(rcs.serviceName, rcs.onUpdateLocalInfo)
	rcs.informer.OnAllTrafficTargetSpecs(rcs.onAllTrafficTargetSpecs)
}

// CheckRegistryURL tries to decode Nacos register request URL parameters.
func (rcs *VMServer) CheckRegistryURL(w http.ResponseWriter, r *http.Request) (serviceName, ip, port string, err error) {

	ip = chi.URLParam(r, "ip")
	port = chi.URLParam(r, "port")
	serviceName = chi.URLParam(r, "serviceName")

	if len(ip) == 0 || len(port) == 0 || len(serviceName) == 0 {
		return ip, port, serviceName, fmt.Errorf("invalid register parameters, ip: %s, port: %s, serviceName: %s",
			ip, port, serviceName)
	}

	serviceName, err = rcs.SplitNacosServiceName(serviceName)

	if serviceName != rcs.serviceName || err != nil {
		return ip, port, serviceName, fmt.Errorf("invalid register serviceName: %s want: %s, err: %v", serviceName, rcs.serviceName, err)
	}
	return ip, port, serviceName, nil
}

func (rcs *VMServer) register(ins *spec.ServiceInstanceSpec, ingressReady, egressReady ReadyFunc) {
	routine := func() (err error) {
		defer func() {
			if err1 := recover(); err1 != nil {
				logger.Errorf("registry center recover from: %v, stack trace:\n%s\n",
					err, debug.Stack())
				err = fmt.Errorf("%v", err1)
			}
		}()

		inReady, eReady := ingressReady(), egressReady()
		if !inReady || !eReady {
			return fmt.Errorf("ingress ready: %v egress ready: %v", inReady, eReady)
		}

		if originIns := rcs.service.GetServiceInstanceSpec(rcs.serviceName, rcs.instanceID); originIns != nil {
			if !needUpdateRecord(originIns, ins) {
				rcs.mutex.Lock()
				rcs.registered = true
				rcs.mutex.Unlock()
				return nil
			}
		}

		ins.Status = spec.ServiceStatusUp
		ins.RegistryTime = time.Now().Format(time.RFC3339)
		rcs.service.PutServiceInstanceSpec(ins)

		rcs.mutex.Lock()
		rcs.registered = true
		rcs.mutex.Unlock()


		return nil
	}

	var firstSucceed bool
	ticker := time.NewTicker(5 * time.Second)
	for {
		err := routine()
		if err != nil {
			logger.Errorf("register failed: %v", err)
		} else if !firstSucceed {
			logger.Infof("register instance spec succeed")
			firstSucceed = true
		}

		select {
		case <-rcs.done:
			ticker.Stop()
			return
		case <-ticker.C:
		}
	}
}

// DiscoveryService gets one service specs with default instance
func (rcs *VMServer) DiscoveryService(serviceName string) (*spec.VMService, error) {
	defer func() {
		if err := recover(); err != nil {
			msg := "registry center recover from: %v, stack trace:\n%s\n"
			logger.Errorf(msg, err, debug.Stack())
		}
	}()

	if !rcs.registered {
		return nil, spec.ErrNoRegisteredYet
	}

	target := rcs.service.GetVMServiceSpec(serviceName)
	if target == nil {
		return nil, spec.ErrServiceNotFound
	}
	return target, nil
}
