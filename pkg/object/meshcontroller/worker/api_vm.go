package worker

func (worker *VMWorker) runAPIServer() {
	var apis []*apiEntry
	switch worker.registryServer.RegistryType {
	default:
		apis = worker.vmNacosAPIs()
	}
	worker.apiServer.registerAPIs(apis)
}
