package worker

import (
	"fmt"
	"github.com/megaease/easegress/pkg/logger"
	"github.com/megaease/easegress/pkg/object/httpserver"
	"github.com/megaease/easegress/pkg/object/meshcontroller/informer"
	"github.com/megaease/easegress/pkg/object/meshcontroller/service"
	"github.com/megaease/easegress/pkg/object/meshcontroller/spec"
	"github.com/megaease/easegress/pkg/object/meshcontroller/storage"
	"github.com/megaease/easegress/pkg/object/trafficcontroller"
	"github.com/megaease/easegress/pkg/supervisor"
)

type VMEgressServer struct {
	*EgressServer
}

// NewVMEgressServer creates an initialized egress server
func NewVMEgressServer(superSpec *supervisor.Spec, super *supervisor.Supervisor,
	serviceName, instanceID string, service *service.Service) *VMEgressServer {
	entity, exists := super.GetSystemController(trafficcontroller.Kind)
	if !exists {
		panic(fmt.Errorf("BUG: traffic controller not found"))
	}

	tc, ok := entity.Instance().(*trafficcontroller.TrafficController)
	if !ok {
		panic(fmt.Errorf("BUG: want *TrafficController, got %T", entity.Instance()))
	}

	inf := informer.NewInformer(storage.New(superSpec.Name(), super.Cluster()), serviceName)

	return &VMEgressServer{
		EgressServer: &EgressServer{
			super:     super,
			superSpec: superSpec,

			inf:         inf,
			tc:          tc,
			namespace:   fmt.Sprintf("%s/%s", superSpec.Name(), "egress"),
			pipelines:   make(map[string]*supervisor.ObjectEntity),
			serviceName: serviceName,
			service:     service,
			instanceID:  instanceID,

			chReloadEvent: make(chan struct{}, 1),
		},
	}
}

// Ready checks Egress HTTPServer has been created or not.
// Not need to check pipelines, cause they will be dynamically added.
func (egs *VMEgressServer) Ready() func() bool {
	return func() bool {
		egs.mutex.RLock()
		defer egs.mutex.RUnlock()
		return egs._ready()
	}

}

func (egs *VMEgressServer) _ready() bool {
	return egs.httpServer != nil
}

// listLocalAndGlobalServices returns services which can be accessed without a traffic control rule
func (egs *VMEgressServer) listLocalAndGlobalServices() map[string]*spec.VMService {
	result := map[string]*spec.VMService{}

	self := egs.service.GetVMServiceSpec(egs.serviceName)
	if self == nil {
		logger.Errorf("cannot find service: %s", egs.serviceName)
		return result
	}

	tenant := egs.service.GetTenantSpec(self.RegisterTenant)
	if tenant != nil {
		for _, name := range tenant.Services {
			if name == egs.serviceName {
				continue
			}

			spec := egs.service.GetVMServiceSpec(name)
			if spec != nil {
				result[name] = spec
			}
		}
	}

	if self.RegisterTenant == spec.GlobalTenant {
		return result
	}

	tenant = egs.service.GetTenantSpec(spec.GlobalTenant)
	if tenant == nil {
		return result
	}

	for _, name := range tenant.Services {
		if name == egs.serviceName {
			continue
		}
		if result[name] != nil {
			continue
		}
		spec := egs.service.GetVMServiceSpec(name)
		if spec != nil {
			result[name] = spec
		}
	}

	return result
}

func (egs *VMEgressServer) listTrafficTargets(lgSvcs map[string]*spec.VMService) []*spec.TrafficTarget {
	var result []*spec.TrafficTarget

	tts := egs.service.ListTrafficTargets()
	for _, tt := range tts {
		// the destination service is a local or global service, which is already accessable
		if lgSvcs[tt.Destination.Name] != nil {
			continue
		}
		for _, s := range tt.Sources {
			if s.Name == egs.serviceName {
				result = append(result, tt)
				break
			}
		}
	}

	return result
}

func (egs *VMEgressServer) listServiceOfTrafficTarget(tts []*spec.TrafficTarget) map[string]*spec.VMService {
	result := map[string]*spec.VMService{}

	for _, tt := range tts {
		name := tt.Destination.Name
		if result[name] != nil {
			continue
		}
		spec := egs.service.GetVMServiceSpec(name)
		if spec == nil {
			logger.Errorf("cannot find service %s of traffic target %s", egs.serviceName, tt.Name)
			continue
		}
		result[name] = spec
	}

	return result
}

// InitEgress initializes the Egress HTTPServer.
func (egs *VMEgressServer) InitEgress(service *spec.Service) error {
	egs.mutex.Lock()
	defer egs.mutex.Unlock()

	if egs.httpServer != nil {
		return nil
	}

	egs.egressServerName = service.EgressHTTPServerName()
	superSpec, err := service.SidecarEgressHTTPServerSpec()
	if err != nil {
		return err
	}

	entity, err := egs.tc.CreateHTTPServerForSpec(egs.namespace, superSpec)
	if err != nil {
		return fmt.Errorf("create http server %s failed: %v", superSpec.Name(), err)
	}
	egs.httpServer = entity

	if err := egs.inf.OnAllServiceSpecs(egs.reloadBySpecs); err != nil {
		// only return err when its type is not `AlreadyWatched`
		if err != informer.ErrAlreadyWatched {
			logger.Errorf("add service spec watching service: %s failed: %v", service.Name, err)
			return err
		}
	}

	if err := egs.inf.OnAllServiceInstanceSpecs(egs.reloadByInstances); err != nil {
		if err != informer.ErrAlreadyWatched {
			logger.Errorf("add service instance spec watching service: %s failed: %v", service.Name, err)
			return err
		}
	}

	admSpec := egs.superSpec.ObjectSpec().(*spec.Admin)
	if admSpec.EnablemTLS() {
		logger.Infof("egress in mtls mode, start listen ID: %s's cert", egs.instanceID)
		if err := egs.inf.OnServerCert(egs.serviceName, egs.instanceID, egs.reloadByCert); err != nil {
			if err != informer.ErrAlreadyWatched {
				logger.Errorf("add server cert spec watching service: %s failed: %v", service.Name, err)
				return err
			}
		}
	}

	if err := egs.inf.OnAllHTTPRouteGroupSpecs(egs.reloadByHTTPRouteGroups); err != nil {
		// only return err when its type is not `AlreadyWatched`
		if err != informer.ErrAlreadyWatched {
			logger.Errorf("add HTTP route group spec watching service: %s failed: %v", service.Name, err)
			return err
		}
	}

	if err := egs.inf.OnAllTrafficTargetSpecs(egs.reloadByTrafficTargets); err != nil {
		// only return err when its type is not `AlreadyWatched`
		if err != informer.ErrAlreadyWatched {
			logger.Errorf("add traffic target spec watching service: %s failed: %v", service.Name, err)
			return err
		}
	}

	if err := egs.inf.OnAllServiceCanaries(egs.reloadByServiceCanaries); err != nil {
		if err != informer.ErrAlreadyWatched {
			logger.Errorf("add service canary watching service: %s failed: %v", service.Name, err)
			return err
		}
	}

	go egs.watch()

	return nil
}

func (egs *VMEgressServer) watch() {
	for range egs.chReloadEvent {
		egs.reload()
	}
}

func (egs *VMEgressServer) reload() {
	lgSvcs := egs.listLocalAndGlobalServices()
	tts := egs.listTrafficTargets(lgSvcs)
	ttSvcs := egs.listServiceOfTrafficTarget(tts)
	groups := egs.listHTTPRouteGroups(tts)

	egs.mutex.Lock()
	defer egs.mutex.Unlock()

	admSpec := egs.superSpec.ObjectSpec().(*spec.Admin)
	var cert, rootCert *spec.Certificate
	if admSpec.EnablemTLS() {
		cert = egs.service.GetServiceInstanceCert(egs.serviceName, egs.instanceID)
		rootCert = egs.service.GetRootCert()
		logger.Infof("egress enable TLS")
	}

	pipelines := make(map[string]*supervisor.ObjectEntity)
	serverName2PipelineName := make(map[string]string)

	canaries := egs.service.ListServiceCanaries()
	createPipeline := func(svc *spec.VMService) {
		instances := egs.listServiceInstances(svc.Name)
		if len(instances) == 0 {
			logger.Warnf("service %s has no instance in UP status", svc.Name)
			return
		}

		pipelineSpec, err := svc.SidecarEgressPipelineSpec(instances, canaries, cert, rootCert)
		if err != nil {
			logger.Errorf("generate sidecar egress pipeline spec for service %s failed: %v", svc.Name, err)
			return
		}
		logger.Infof("service: %s visit: %s pipeline init ok", egs.serviceName, svc.Name)

		entity, err := egs.tc.CreateHTTPPipelineForSpec(egs.namespace, pipelineSpec)
		if err != nil {
			logger.Errorf("update http pipeline failed: %v", err)
			return
		}
		pipelines[svc.Name] = entity
		serverName2PipelineName[svc.Name] = pipelineSpec.Name()
	}

	for _, svc := range lgSvcs {
		createPipeline(svc)
	}
	for _, svc := range ttSvcs {
		createPipeline(svc)
	}

	httpServerSpec := egs.httpServer.Spec().ObjectSpec().(*httpserver.Spec)
	httpServerSpec.Rules = nil

	for serviceName := range lgSvcs {
		rule := &httpserver.Rule{
			Paths: []*httpserver.Path{
				{
					PathPrefix: "/",
					Headers: []*httpserver.Header{
						{
							Key: egressRPCKey,
							// Value should be the service name
							Values: []string{serviceName},
						},
					},
					// this name should be the pipeline full name
					Backend: serverName2PipelineName[serviceName],
				},
			},
		}

		// for matching only host name request
		//   1) try exactly matching
		//   2) try matching with regexp
		ruleHost := &httpserver.Rule{
			Host:       serviceName,
			HostRegexp: egs.buildHostRegex(serviceName),
			Paths: []*httpserver.Path{
				{
					PathPrefix: "/",
					// this name should be the pipeline full name
					Backend: serverName2PipelineName[serviceName],
				},
			},
		}

		httpServerSpec.Rules = append(httpServerSpec.Rules, rule, ruleHost)
	}

	for _, tt := range tts {
		pipelineName := serverName2PipelineName[tt.Destination.Name]
		if pipelineName == "" {
			continue
		}

		for _, r := range tt.Rules {
			var matches []spec.HTTPMatch
			if len(r.Matches) == 0 {
				matches = groups[r.Name].Matches
			} else {
				allMatches := groups[r.Name].Matches
				for _, name := range r.Matches {
					for i := range allMatches {
						if allMatches[i].Name == name {
							matches = append(matches, allMatches[i])
						}
					}
				}
			}

			rules := egs.buildMuxRule(pipelineName, tt.Destination.Name, matches)
			httpServerSpec.Rules = append(httpServerSpec.Rules, rules...)
		}
	}

	builder := newHTTPServerSpecBuilder(egs.egressServerName, httpServerSpec)
	superSpec, err := supervisor.NewSpec(builder.yamlConfig())
	if err != nil {
		logger.Errorf("new spec for %s failed: %v", err)
		return
	}
	entity, err := egs.tc.UpdateHTTPServerForSpec(egs.namespace, superSpec)
	if err != nil {
		logger.Errorf("update http server %s failed: %v", egs.egressServerName, err)
		return
	}

	// update local storage
	egs.pipelines = pipelines
	egs.httpServer = entity
}
