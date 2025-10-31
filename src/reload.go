package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"text/template"
	"time"

	nplus "github.com/nginxinc/nginx-plus-go-client/client"
	"github.com/sirupsen/logrus"

	runtime_models "github.com/haproxytech/client-native/v5/models"
	runtime_api "github.com/haproxytech/client-native/v5/runtime"
	runtime_options "github.com/haproxytech/client-native/v5/runtime/options"
)

type NamespaceRenderingData struct {
	LeaderVHost string
	Leader      LeaderController
	RoutingTag  string `json:"-"`
}

type RenderingData struct {
	Xproxy                                string
	ProxyPlatform                         string
	LeftDelimiter                         string                            `json:"-" toml:"left_delimiter"`
	RightDelimiter                        string                            `json:"-" toml:"right_delimiter"`
	MaxFailsUpstream                      *int                              `json:"max_fails,omitempty"`
	FailTimeoutUpstream                   string                            `json:"fail_timeout,omitempty"`
	SlowStartUpstream                     string                            `json:"slow_start,omitempty"`
	HaproxyAddServerAttributesString      string                            `json:"-" toml:"haproxy_add_server_attributes_string"`
	HaproxyAddServerSSLAttributesString   string                            `json:"-" toml:"haproxy_add_server_ssl_attributes_string"`
	HaproxyServerNamePrefix               string                            `json:"-" toml:"haproxy_server_name_prefix"`
	HaproxyServerNameHostPortSeparator    string                            `json:"-" toml:"haproxy_server_name_host_port_delimiter"`
	HaproxyBackendNameSeparator           string                            `json:"-" toml:"haproxy_backend_name_separator"`
	HaproxyBackendIncludeRoutingTagSuffix bool                              `json:"-" toml:"haproxy_backend_include_routing_tag_suffix"`
	Namespaces                            map[string]NamespaceRenderingData `json:"namespaces"`
	Apps                                  map[string]App
}

func reload() error {
	start := time.Now()
	var err error
	data := RenderingData{}
	createRenderingData(&data)
	config.LastUpdates.LastSync = time.Now()

	var upstreamUpdateAPIEnabled bool

	if (config.ProxyPlatform == "nginx") && (len(config.Nginxplusapiaddr) == 0 || config.Nginxplusapiaddr == "") {
		logger.Debug("Platform: " + config.ProxyPlatform + " API addr: " + config.Nginxplusapiaddr)
		//Nginx plus http_api is disabled
		upstreamUpdateAPIEnabled = false
	} else if (config.ProxyPlatform == "haproxy") && (len(config.HaproxySocketAddr) == 0 || config.HaproxySocketAddr == "") {
		logger.Debug("Platform: " + config.ProxyPlatform + " Socket addr: " + config.HaproxySocketAddr)
		logger.Debug("Runtime API add server default-server attributes:" + config.HaproxyAddServerAttributesString)
		logger.Debug("Runtime API add server ssl attributes:" + config.HaproxyAddServerSSLAttributesString)
		//HAProxy Runtime API is disabled
		upstreamUpdateAPIEnabled = false
	} else {
		upstreamUpdateAPIEnabled = true
	}

	if !upstreamUpdateAPIEnabled {
		logger.Debug("Runtime API calls to update upstreams are disabled")

		health.Lock()
		health.UpstreamUpdatesViaAPI.Healthy = true
		health.UpstreamUpdatesViaAPI.Message = "OK: Not in use, full reloads are enabled"
		health.Unlock()

		// Any use of runtime API is disabled, so we must perform a full reload.
		// We still need to calculate backend names to update the database.
		currentBackendNames := make(map[string]bool)
		for _, app := range data.Apps {
			if app.Vhost != "" {
				var backendName string
				for groupName, groupData := range app.Groups {
					logrus.Debug("Group: " + groupName + " Data: " + fmt.Sprintf("%+v", groupData))
					backendName = generateStableBackendName(app, config.ProxyPlatform, groupName)
					if backendName != "" {
						currentBackendNames[backendName] = true
					}
				}
			}
		}
		err = updateAndReloadConfig(&data, false, currentBackendNames)
		if err != nil {
			logger.WithFields(logrus.Fields{
				"error": err.Error(),
			}).Error("unable to reload " + config.ProxyPlatform + " config")
			go statsCount("reload.failed", 1)
			go countFailedReloads.Inc()
			return err
		}
	} else {
		logger.Debug("Runtime API calls to update upstreams are enabled")
		//Use of runtime API is enabled
		//For HAProxy, config is generated but not loaded even when reload is disabled as there is no other way to persist state across reloads
		//For Nginx+, ngx http_api maintains it's own state files if referenced in the running nginx config. Hence no templating is done at all when reload is disabled
		if ConfigReloadDisabled {
			logger.Debug("Nginx: Template reload has been disabled")
		} else {
			vhosts := db.ReadAllKnownVhosts()
			lastKnownVhosts := db.ReadLastKnownVhosts()

			// Generate a set of current backend names to detect changes.
			currentBackendNames := make(map[string]bool)
			for _, app := range data.Apps {
				if app.Vhost != "" {
					var backendName string
					for groupName, groupData := range app.Groups {
						logrus.Debug("Group: " + groupName + " Data: " + fmt.Sprintf("%+v", groupData))
						backendName = generateStableBackendName(app, config.ProxyPlatform, groupName)
						if backendName != "" {
							currentBackendNames[backendName] = true
						}
					}
				}
			}
			lastKnownBackendNames := db.ReadLastKnownBackends()

			// A reload is needed if vhosts have changed OR if the set of backend names has changed.
			// A change in backend names implies a new routingTagValue has been introduced,
			// requiring a new backend block in the config.
			if !reflect.DeepEqual(vhosts, lastKnownVhosts) || !reflect.DeepEqual(currentBackendNames, lastKnownBackendNames) {
				if !reflect.DeepEqual(vhosts, lastKnownVhosts) {
					logger.Info("Vhost changes detected. Need to reload config")
				} else {
					logger.Info("Routing tag changes detected, resulting in new backend names. Need to reload config")
				}

				err = updateAndReloadConfig(&data, false, currentBackendNames)
				if err != nil {
					logger.WithFields(logrus.Fields{
						"error": err.Error(),
					}).Error("unable to update and reload " + config.ProxyPlatform + " config. Runtime api calls to update upstreams will be skipped.")
					return err
				}
			} else {
				logger.Debug("No changes detected in vhosts or backend names. No config update is necessary. Upstream updates will happen via " + config.ProxyPlatform + " apis")
			}
		}
		logger.Debug("Updating upstreams via " + config.ProxyPlatform + " api")
		if config.ProxyPlatform == "nginx" {
			err = nginxPlus(&data)
		} else if config.ProxyPlatform == "haproxy" {
			//For HAProxy, config is generated but not loaded even when reload is disabled as there is not other way to persist state across reloads
			logger.Debug("HAProxy: Updating config without reload")
			updateWithoutReloadConfig(&data)
			// Create a context with a timeout for the API call.
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(config.apiTimeout)*time.Second)
			defer cancel()
			err = haproxyRuntimeAPI(&data, ctx)
		}

		// Update health status based on the result of the API call
		if err != nil {
			updateHealthForUpstreamUpdateAPI(false, err.Error())
			logger.WithFields(logrus.Fields{
				"error": err.Error(),
			}).Error("unable to update upstreams via " + config.ProxyPlatform + " api")
			go statsCount("reload.failed", 1)
			go countFailedReloads.Inc()
		} else {
			updateHealthForUpstreamUpdateAPI(true, "OK")
		}
	}
	if err != nil {
		logger.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Error("unable to generate nginx config")
		go statsCount("reload.failed", 1)
		go countFailedReloads.Inc()
		return err
	}
	elapsed := time.Since(start)
	logger.WithFields(logrus.Fields{
		"took": elapsed,
	}).Debug("reload worker completed")
	return nil

}

func updateHealthForUpstreamUpdateAPI(status bool, message string) {
	health.Lock()
	health.UpstreamUpdatesViaAPI.Healthy = status
	health.UpstreamUpdatesViaAPI.Message = message
	health.Unlock()
}

func updateWithoutReloadConfig(data *RenderingData) error {
	logger.Debug("Updating config without reload")
	config.LastUpdates.LastSync = time.Now()
	err := writeConf(data)
	if err != nil {
		logger.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Error("unable to generate " + config.ProxyPlatform + " config")
		go statsCount("reload.failed", 1)
		go countFailedReloads.Inc()
		return err
	}
	config.LastUpdates.LastConfigValid = time.Now()
	return nil
}

func updateAndReloadConfig(data *RenderingData, reloadDisabled bool, currentBackendNames map[string]bool) error {
	logger.Debug("Updating config with reload")
	start := time.Now()
	config.LastUpdates.LastSync = time.Now()
	vhosts := db.ReadAllKnownVhosts()
	err := writeConf(data)
	if err != nil {
		go countFailedReloads.Inc()
		go statsCount("reloads_failed", 1)
		logger.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Error("unable to write " + config.ProxyPlatform + " config")
		return err
	}
	config.LastUpdates.LastConfigValid = time.Now()

	if !reloadDisabled {
		if config.ProxyPlatform == "nginx" {
			err = reloadNginx()
		} else if config.ProxyPlatform == "haproxy" {
			err = reloadHaproxy()
		}

		if err != nil {
			logger.WithFields(logrus.Fields{
				"error": err.Error(),
			}).Error("unable to reload nginx")
			go statsCount("reload.failed", 1)
			go countFailedReloads.Inc()
		} else {
			elapsed := time.Since(start)
			go countSuccessfulReloads.Inc()
			go statsCount("reloads_successful", 1)
			go observeReloadTimeMetric(elapsed)
			go statsTiming("reload_duration", elapsed)
			config.LastUpdates.LastProxyProgramReload = time.Now()
			db.UpdateLastKnownVhosts(vhosts)
			db.UpdateLastKnownBackends(currentBackendNames)
		}
	} else if reloadDisabled {
		logger.Info("Config reload has been disabled. Not reloading " + config.ProxyPlatform + " even after vhost/backend changes")
	}
	return nil
}

func createRenderingData(data *RenderingData) {
	namespaceData := db.ReadAllNamespace()
	staticData := db.ReadStaticData()

	data.Xproxy = staticData.Xproxy
	data.ProxyPlatform = staticData.ProxyPlatform
	data.LeftDelimiter = staticData.LeftDelimiter
	data.RightDelimiter = staticData.RightDelimiter
	data.FailTimeoutUpstream = staticData.FailTimeoutUpstream
	data.MaxFailsUpstream = staticData.MaxFailsUpstream
	data.SlowStartUpstream = staticData.SlowStartUpstream
	data.HaproxyAddServerAttributesString = staticData.HaproxyAddServerAttributesString
	data.HaproxyAddServerSSLAttributesString = staticData.HaproxyAddServerSSLAttributesString
	data.HaproxyServerNamePrefix = staticData.HaproxyServerNamePrefix
	data.HaproxyServerNameHostPortSeparator = staticData.HaproxyServerNameHostPortSeparator
	data.HaproxyBackendNameSeparator = staticData.HaproxyBackendNameSeparator
	data.HaproxyBackendIncludeRoutingTagSuffix = staticData.HaproxyBackendIncludeRoutingTagSuffix
	data.Namespaces = make(map[string]NamespaceRenderingData)

	allApps := make(map[string]App)

	for name, nmData := range namespaceData {
		data.Namespaces[name] = NamespaceRenderingData{
			LeaderVHost: nmData.Drove.LeaderVHost,
			Leader:      nmData.Leader,
			RoutingTag:  nmData.Drove.RoutingTag,
		}

		//Merging App if already exists
		for appId, appData := range nmData.Apps {
			appData.RoutingTagKey = nmData.Drove.RoutingTag
			if existingAppData, ok := allApps[appId]; ok {
				//Appending hosts
				existingAppData.Hosts = append(existingAppData.Hosts, appData.Hosts...)

				//adding same as that of app refresh logic in drove client
				if existingAppData.Tags == nil {
					existingAppData.Tags = make(map[string]string)
				}
				if appData.Tags != nil {
					for tagK, tagV := range appData.Tags {
						existingAppData.Tags[tagK] = tagV
					}
				}

				if existingAppData.Groups == nil {
					existingAppData.Groups = make(map[string]HostGroup)
				}

				if appData.Groups != nil {
					for groupName, groupData := range appData.Groups {
						if existingGroup, ok := existingAppData.Groups[groupName]; ok {

							//Appending hosts
							existingGroup.Hosts = append(existingGroup.Hosts, groupData.Hosts...)

							if existingGroup.Tags == nil {
								existingGroup.Tags = make(map[string]string)
							}

							if groupData.Tags != nil {
								for tn, tv := range groupData.Tags {
									existingGroup.Tags[tn] = tv
								}
							}
							existingAppData.Groups[groupName] = existingGroup
						} else {
							existingAppData.Groups[groupName] = groupData
						}
					}
				}
				allApps[appId] = existingAppData
			} else {
				allApps[appId] = appData
			}
		}
	}
	data.Apps = allApps
	logger.WithFields(logrus.Fields{
		"data": data,
	}).Trace("Rendering data generated")
	return
}

func renderConfigFromTemplate(tmpl *template.Template, data *RenderingData, file *os.File) error {
	start := time.Now()
	err := tmpl.Execute(file, data)
	duration := time.Since(start)
	resultLabel := "success"
	if err != nil {
		resultLabel = "error"
	}
	statsTimingVec("template_render_duration", duration, resultLabel)
	return err
}

func writeConf(data *RenderingData) error {
	config.RLock()
	defer config.RUnlock()

	template, err := getTmpl(TemplatePath)
	if err != nil {
		return err
	}

	parent := filepath.Dir(ConfigPath)
	tmpFile, err := os.CreateTemp(parent, "."+config.ProxyPlatform+".conf.tmp-")
	if err != nil {
		return err
	}
	defer tmpFile.Close()
	defer os.Remove(tmpFile.Name())
	lastConfig = tmpFile.Name()

	err = renderConfigFromTemplate(template, data, tmpFile)
	if err != nil {
		health.Lock()
		health.Config.Healthy = false
		health.Config.Message = err.Error()
		health.Unlock()
		return err
	}
	config.LastUpdates.LastConfigRendered = time.Now()
	err = checkConf(tmpFile.Name())
	health.Lock()
	if err != nil {
		health.Config.Healthy = false
		health.Config.Message = err.Error()
		logger.Error("Error in config generated")
	} else {
		health.Config.Healthy = true
		health.Config.Message = "OK"
	}
	health.Unlock()
	if err != nil {
		return err
	}

	logger.WithFields(logrus.Fields{
		"file": ConfigPath,
	}).Info("Writing new config")
	err = os.Rename(tmpFile.Name(), ConfigPath)
	if err != nil {
		return err
	}
	lastConfig = ConfigPath
	return nil
}

func IsUnixSocketAddr(addr string) bool {
	if strings.HasPrefix(addr, "ipv4@") || strings.HasPrefix(addr, "ipv6@") {
		return false
	}
	if strings.Contains(addr, ":") {
		return false
	}
	return true
}

func newHAProxyClient(ctx context.Context) (runtime_api.Runtime, error) {
	haproxySocket := config.HaproxySocketAddr
	logger.WithField("haproxy_socket", haproxySocket).Debug("Preparing to connect to HAProxy runtime API")

	if haproxySocket == "" {
		return nil, errors.New("HAProxy socket address is not configured")
	}

	if IsUnixSocketAddr(haproxySocket) {
		if _, err := os.Stat(haproxySocket); os.IsNotExist(err) {
			return nil, fmt.Errorf("HAProxy socket file does not exist: %s", haproxySocket)
		}
	}

	runtimeClient, err := runtime_api.New(ctx, runtime_options.Sockets(map[int]string{1: haproxySocket}))
	if err != nil {
		return nil, fmt.Errorf("error initializing HAProxy runtime client: %w", err)
	}

	// Test the connection to ensure the runtime API is responsive.
	if _, err := runtimeClient.GetInfo(); err != nil {
		return nil, fmt.Errorf("error connecting to HAProxy socket: %w", err)
	}

	logger.Info("Successfully connected to HAProxy runtime API")
	return runtimeClient, nil
}

func isHTTPHostGroup(hosts []Host) bool {
	for _, host := range hosts {
		if host.PortType != "http" && host.PortType != "https" {
			return false
		}
	}
	return true
}

func haproxyRuntimeAPI(data *RenderingData, ctx context.Context) error {
	start := time.Now()
	var resultLabel string
	defer func() {
		duration := time.Since(start)
		haproxyReconcileAllBackendsDuration.WithLabelValues(resultLabel).Observe(duration.Seconds())
		statsTimingVec("haproxy_runtime_api_duration", duration, resultLabel)
	}()

	config.RLock()
	defer config.RUnlock()

	runtimeClient, err := newHAProxyClient(ctx)
	if err != nil {
		resultLabel = "error"
		updateHealthForUpstreamUpdateAPI(false, err.Error())
		return err
	}

	// Aggregate all desired hosts for each unique backend from our configuration. We don't want to make excess API calls
	backendsToReconcile := make(map[string][]Host)
	for _, app := range data.Apps {
		if app.Vhost == "" {
			continue
		}
		for groupName, groupData := range app.Groups {
			if len(groupData.Hosts) == 0 {
				continue
			}

			if !isHTTPHostGroup(groupData.Hosts) {
				logger.WithFields(logrus.Fields{
					"Vhost": app.Vhost,
					"group": groupName,
					"hosts": groupData.Hosts,
				}).Warning("Non-HTTP/HTTPS host group detected. Skipping HAProxy runtime API update for this group.")
				continue
			}

			backendName := generateStableHaproxyBackendName(app, groupName)
			if backendName != "" {
				backendsToReconcile[backendName] = append(backendsToReconcile[backendName], groupData.Hosts...)
			}

		}
	}

	// Reconcile each unique backend. GetServersState (i.e. show servers state %backendName ) can only be called once per backend. Runtime API library doesn't return backend names
	// when calling GetServersState as models.RuntimeServers is a list of []*RuntimeServer without backend context returned by models.ParseRuntimeServer()
	// If number of backends is large, this could take a while and instead we will use equivalent of "show servers state <all>" which returns all backends and servers in one call
	// we will only call GetServersState if we see a diff in server names expected for a backend

	//For clusters with small number of backends, calling GetServersState for each backend is not a big overhead
	//but for a lot of backends, this can take a lot of time and we could have stale data for backends processed later in the loop
	//E.g. For a heavily loaded HAProxy with 1000 backends and >4000 RPS, calling GetServersState for each backend could take upwards of 200 seconds
	//So we will call GetServersState only if we don't find the backend in aggregated call to our own implementation of GetServersStateWithBackend
	//This way we optimize for both small and large number of backends

	//Upstream runtime doesn't expose the backend name when calling GetServersState API
	//so we have our own implementation which calls "show servers state" command and parses the output to get servers grouped by backend name

	//We could also use GetStats API but that would require more processing to convert stats to server state
	//and stats don't show servers in maintenance mode unless they are enabled. So a disabled server in maintenance mode
	//would not be visible in stats output but it is visible in "show servers state" output

	//Despite the fact that "show servers state" is reliable, making the use configurable in case of any issues of changes to models in future versions of HAProxy client runtime
	logger.WithField("count", len(backendsToReconcile)).Debug("Reconciling all unique HAProxy backends")
	reconciledBackends := make(map[string]bool)
	reconciliationFailedBackends := make(map[string]bool)
	allServersState := make(map[string]runtime_models.RuntimeServers)
	if !config.HaproxyDisableLargeBackendCountOptimisation {
		logger.Debug("HAProxy large backend count optimisation is enabled. Using aggregated call to get all backends and servers state")
		allServersState, err = haproxyClientGetServersStateWithBackend(runtimeClient)
		if err != nil {
			resultLabel = "error"
			updateHealthForUpstreamUpdateAPI(false, err.Error())
			logger.WithFields(logrus.Fields{
				"error": err,
			}).Error("Failed to get HAProxy servers state for all backends")
			haproxyAPICallsFailed.WithLabelValues("get_servers_state_all_backends").Inc()
			statsCountVec("haproxy_api_calls_failed_total", 1, "get_servers_state_all_backends")
			return fmt.Errorf("failed to get HAProxy servers state for all backends: %w", err)
		}
		haproxyAPICallsSuccessful.WithLabelValues("get_servers_state_all_backends").Inc()
		statsCountVec("haproxy_api_calls_successful_total", 1, "get_servers_state_all_backends")
	}

	for backend, hosts := range backendsToReconcile {

		currentServersForBackend := []*runtime_models.RuntimeServer{}
		err := error(nil)
		// If we have state for this backend from the aggregated call, use it directly.
		// This avoids making an additional API call per backend.
		if !config.HaproxyDisableLargeBackendCountOptimisation || allServersState[backend] != nil {
			currentServersForBackend = allServersState[backend]
		} else {
			if allServersState[backend] != nil {
				logger.Warn("Consider disabling large backend count optimisation with haproxy_disable_large_backend_count_optimisation set to true")
			}
			logger.WithField("backend", backend).Debug("No existing servers found for backend in aggregated state. Trying to get state for the backend directly")
			currentServersForBackend, err = runtimeClient.GetServersState(backend)
		}

		if err != nil {
			reconciliationFailedBackends[backend] = true
			// This error is often not fatal; it can mean the backend doesn't exist yet.
			logger.WithFields(logrus.Fields{
				"backend": backend,
				"error":   err,
			}).Warning("Could not get servers for backend, it may be new. Proceeding with reconciliation.")
			haproxyAPICallsFailed.WithLabelValues("get_servers_state").Inc()
			statsCountVec("haproxy_api_calls_failed_total", 1, "get_servers_state")
			// Pass an empty slice so reconciliation can proceed to add servers.
			currentServersForBackend = []*runtime_models.RuntimeServer{}
		} else {
			haproxyAPICallsSuccessful.WithLabelValues("get_servers_state").Inc()
			statsCountVec("haproxy_api_calls_successful_total", 1, "get_servers_state")
		}

		if err := reconcileHAProxyBackend(runtimeClient, backend, hosts, currentServersForBackend); err != nil {
			reconciliationFailedBackends[backend] = true
			logger.WithFields(logrus.Fields{
				"backend": backend,
				"error":   err,
			}).Error("Failed to reconcile HAProxy backend")
			// Continue with other backends instead of failing completely
		} else {
			reconciledBackends[backend] = true
		}
	}
	if len(reconciliationFailedBackends) > 0 || len(reconciledBackends) == 0 {
		resultLabel = "error"
		if len(reconciliationFailedBackends) > 0 {
			logger.WithField("failed_backends", reconciliationFailedBackends).Error("Failed to reconcile some HAProxy backends")
			updateHealthForUpstreamUpdateAPI(false, errors.New("failed to reconcile some HAProxy backends: "+fmt.Sprintf("%v", reconciliationFailedBackends)).Error())
		}
		if len(reconciledBackends) == 0 {
			logger.WithField("reconciled_backends", reconciledBackends).Error("Failed to reconcile any HAProxy backends")
			updateHealthForUpstreamUpdateAPI(false, errors.New("failed to reconcile any HAProxy backends").Error())
		}
		return errors.New("Reconciliation failed for some or all HAProxy backends")
	}

	resultLabel = "success"
	logger.Info("Successfully reconciled all HAProxy backends")
	updateHealthForUpstreamUpdateAPI(true, "OK")

	return nil
}

func reconcileHAProxyBackend(client runtime_api.Runtime, backend string, desiredHosts []Host, currentServers []*runtime_models.RuntimeServer) error {
	var errs []string
	logger.WithField("backend", backend).Debug("Reconciling HAProxy backend")

	desiredServerMap := haproxyBuildDesiredServerMap(desiredHosts)
	currentServerMap := haproxyBuildCurrentServerMap(currentServers)

	if haproxyAreServerMapsIdentical(desiredServerMap, currentServerMap) {
		logger.WithField("backend", backend).Debug("HAProxy backend is already in the desired state. No changes needed.")
		return nil
	}

	if err := haproxyAddOrUpdateServers(client, backend, desiredServerMap, currentServerMap); err != nil {
		errs = append(errs, fmt.Sprintf("add/update servers: %v", err))
	}
	//add or update servers first to avoid downtime in case of complete replacement of servers
	if err := haproxyRemoveStaleServers(client, backend, currentServers, desiredServerMap); err != nil {
		errs = append(errs, fmt.Sprintf("remove stale servers: %v", err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to reconcile HAProxy backend: %v", errs)
	}

	logger.WithField("backend", backend).Info("Successfully reconciled HAProxy backend")
	return nil
}

func haproxyAreServerMapsIdentical(desiredMap map[string]Host, currentMap map[string]runtime_models.RuntimeServer) bool {
	if len(desiredMap) != len(currentMap) {
		return false
	}

	for serverName, desiredHost := range desiredMap {
		currentServer, exists := currentMap[serverName]
		if !exists {
			return false
		}

		// Resolve desired hostname to IP for a reliable comparison with HAProxy's runtime state.
		desiredIP, err := resolveWithIPFallback(desiredHost.Host)
		if err != nil {
			logger.WithFields(logrus.Fields{"hostname": desiredHost.Host, "error": err}).Warning("Failed to resolve hostname; using hostname for comparison")
		}

		portMatches := currentServer.Port != nil && *currentServer.Port == int64(desiredHost.Port)
		addressMatches := currentServer.Address == desiredIP
		adminStateMatches := currentServer.AdminState == "ready"
		// We only check for 'up' or 'maint' as operational state can fluctuate (e.g., 'going down').
		// A server in 'maint' is operationally down but administratively configured, so we consider it a match if we want it 'up'.
		opStateMatches := currentServer.OperationalState == "up" || currentServer.OperationalState == "maint"

		if !(portMatches && addressMatches && adminStateMatches && opStateMatches) {
			return false // Properties do not match.
		}
	}

	return true
}

func haproxyBuildDesiredServerMap(desiredHosts []Host) map[string]Host {
	serverMap := make(map[string]Host)
	for _, host := range desiredHosts {
		serverName := generateStableHaproxyServerName(host)
		serverMap[serverName] = host
	}
	return serverMap
}

func haproxyBuildCurrentServerMap(currentServers []*runtime_models.RuntimeServer) map[string]runtime_models.RuntimeServer {
	serverMap := make(map[string]runtime_models.RuntimeServer)
	for _, srv := range currentServers {
		serverMap[srv.Name] = *srv
	}
	return serverMap
}

func haproxyRemoveStaleServers(client runtime_api.Runtime, backend string, currentServers []*runtime_models.RuntimeServer, desiredServerMap map[string]Host) error {
	var errs []string
	for _, srv := range currentServers {
		if _, exists := desiredServerMap[srv.Name]; !exists {
			logger.WithFields(logrus.Fields{"backend": backend, "server": srv.Name}).Info("Disabling and deleting stale server")
			// Set server state to 'maint' before deleting.
			if err := client.SetServerState(backend, srv.Name, "maint"); err != nil {
				haproxyAPICallsFailed.WithLabelValues("set_server_state_maint").Inc()
				statsCountVec("haproxy_api_calls_failed_total", 1, "set_server_state_maint")
				logger.WithFields(logrus.Fields{"backend": backend, "server": srv.Name, "error": err}).Error("Failed to disable server")
				errs = append(errs, fmt.Sprintf("disable %s: %v", srv.Name, err))
			} else {
				haproxyAPICallsSuccessful.WithLabelValues("set_server_state_maint").Inc()
				statsCountVec("haproxy_api_calls_successful_total", 1, "set_server_state_maint")
			}

			if err := client.DeleteServer(backend, srv.Name); err != nil {
				haproxyAPICallsFailed.WithLabelValues("delete_server").Inc()
				statsCountVec("haproxy_api_calls_failed_total", 1, "delete_server")
				logger.WithFields(logrus.Fields{"backend": backend, "server": srv.Name, "error": err}).Error("Failed to delete server")
				errs = append(errs, fmt.Sprintf("delete %s: %v", srv.Name, err))
			} else {
				haproxyAPICallsSuccessful.WithLabelValues("delete_server").Inc()
				statsCountVec("haproxy_api_calls_successful_total", 1, "delete_server")
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("Errors disabling stale servers: %s", strings.Join(errs, "; "))
	}
	return nil
}

func haproxyAddOrUpdateServers(client runtime_api.Runtime, backend string, desiredServerMap map[string]Host, currentServerMap map[string]runtime_models.RuntimeServer) error {
	var errs []string
	for serverName, host := range desiredServerMap {
		if _, exists := currentServerMap[serverName]; !exists {
			if err := haproxyAddNewServer(client, backend, serverName, host); err != nil {
				errs = append(errs, fmt.Sprintf("add %s: %v", serverName, err))
			}
		} else {
			if err := haproxyUpdateExistingServer(client, backend, serverName, host, currentServerMap[serverName]); err != nil {
				errs = append(errs, fmt.Sprintf("update %s: %v", serverName, err))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("Errors in Add/Update servers: %s", strings.Join(errs, "; "))
	}
	return nil
}

//settings from default-server statement in config are not applied when adding server via runtime api
//all default-server should be added explictly here

// only use IP while adding server, not fqdn. HAProxy resolves the hostname only at startup/reload and dynamic servers don't support fqdn resolution
func haproxyAddNewServer(client runtime_api.Runtime, backend, serverName string, host Host) error {
	logger.WithFields(logrus.Fields{"backend": backend, "server": serverName}).Info("Adding new server")

	desiredIP, err := resolveWithIPFallback(host.Host)
	if err != nil {
		logger.WithFields(logrus.Fields{"hostname": host.Host, "error": err}).Warning("Failed to resolve hostname; using hostname for server addition")
	}

	// Resolve desired hostname to IP for HAProxy server addition.

	if host.PortType != "http" && host.PortType != "https" {
		logger.WithFields(logrus.Fields{"backend": backend, "server": serverName}).Warning("Non-HTTP/HTTPS port type detected. Skipping addition of this server via HAProxy runtime API.")
		return nil
	}

	attributes_string := config.HaproxyAddServerAttributesString
	logger.WithFields(logrus.Fields{"attributes": attributes_string, "backend": backend, "server": serverName}).Debug("Adding attributes to server")

	if host.PortType == "https" {
		logger.WithFields(logrus.Fields{"backend": backend, "server": serverName}).Debug("HTTPS port type detected. HAProxy runtime API does not support adding SSL parameters via runtime. Ensure SSL settings like ciphers are configured in the static config.")
		attributes_string = fmt.Sprintf("%s %s", attributes_string, config.HaproxyAddServerSSLAttributesString)
		logger.WithFields(logrus.Fields{"attributes": attributes_string, "backend": backend, "server": serverName}).Info("Adding SSL attributes to server")

	}

	if err := client.AddServer(backend, serverName, fmt.Sprintf("%s:%d %s", desiredIP, host.Port, attributes_string)); err != nil {
		haproxyAPICallsFailed.WithLabelValues("add_server").Inc()
		statsCountVec("haproxy_api_calls_failed_total", 1, "add_server")
		logger.WithFields(logrus.Fields{"backend": backend, "server": serverName, "error": err}).Error("Failed to add server")
		return err
	}
	haproxyAPICallsSuccessful.WithLabelValues("add_server").Inc()
	statsCountVec("haproxy_api_calls_successful_total", 1, "add_server")

	haproxyAPICallsSuccessful.WithLabelValues("set_server_addr").Inc()
	statsCountVec("haproxy_api_calls_successful_total", 1, "set_server_addr")

	if err := client.SetServerState(backend, serverName, "ready"); err != nil {
		haproxyAPICallsFailed.WithLabelValues("set_server_state_ready").Inc()
		statsCountVec("haproxy_api_calls_failed_total", 1, "set_server_state_ready")
		logger.WithFields(logrus.Fields{"backend": backend, "server": serverName, "error": err}).Error("Failed to set new server state to ready")
		return err
	}
	haproxyAPICallsSuccessful.WithLabelValues("set_server_state_ready").Inc()
	statsCountVec("haproxy_api_calls_successful_total", 1, "set_server_state_ready")

	return nil
}

func haproxyUpdateExistingServer(client runtime_api.Runtime, backend, serverName string, host Host, currentServer runtime_models.RuntimeServer) error {
	// Resolve desired hostname to IP for a comparison with HAProxy's runtime state.
	desiredIP, err := resolveWithIPFallback(host.Host)
	if err != nil {
		logger.WithFields(logrus.Fields{"hostname": host.Host, "error": err}).Warning("Failed to resolve hostname; using hostname for comparison")
	}

	portMatches := currentServer.Port != nil && *currentServer.Port == int64(host.Port)
	if currentServer.Address == desiredIP && portMatches && currentServer.AdminState == "ready" && currentServer.OperationalState == "up" {
		logger.WithFields(logrus.Fields{"backend": backend, "server": serverName}).Trace("Server is up-to-date, no update needed")
		return nil
	}

	logger.WithFields(logrus.Fields{
		"backend":           backend,
		"server":            serverName,
		"current_address":   currentServer.Address,
		"current_port":      *currentServer.Port,
		"current_admin":     currentServer.AdminState,
		"current_operstate": currentServer.OperationalState,
		"desired_address":   desiredIP,
		"desired_port":      host.Port,
	}).Info("Updating existing server")

	var errs []string

	if err := client.SetServerAddr(backend, serverName, desiredIP, int(host.Port)); err != nil {
		haproxyAPICallsFailed.WithLabelValues("set_server_addr").Inc()
		statsCountVec("haproxy_api_calls_failed_total", 1, "set_server_addr")
		logger.WithFields(logrus.Fields{"backend": backend, "server": serverName, "error": err}).Error("Failed to set server address")
		errs = append(errs, fmt.Sprintf("set address: %v", err))
	} else {
		haproxyAPICallsSuccessful.WithLabelValues("set_server_addr").Inc()
		statsCountVec("haproxy_api_calls_successful_total", 1, "set_server_addr")
	}

	if err := client.SetServerHealth(backend, serverName, "up"); err != nil {
		haproxyAPICallsFailed.WithLabelValues("set_server_health_up").Inc()
		statsCountVec("haproxy_api_calls_failed_total", 1, "set_server_health_up")
		logger.WithFields(logrus.Fields{"backend": backend, "server": serverName, "error": err}).Error("Failed to set server health to up")
		errs = append(errs, fmt.Sprintf("set health: %v", err))
	} else {
		haproxyAPICallsSuccessful.WithLabelValues("set_server_health_up").Inc()
		statsCountVec("haproxy_api_calls_successful_total", 1, "set_server_health_up")
	}

	if err := client.SetServerState(backend, serverName, "ready"); err != nil {
		haproxyAPICallsFailed.WithLabelValues("set_server_state_ready").Inc()
		statsCountVec("haproxy_api_calls_failed_total", 1, "set_server_state_ready")
		logger.WithFields(logrus.Fields{"backend": backend, "server": serverName, "error": err}).Error("Failed to set server state to ready")
		errs = append(errs, fmt.Sprintf("set state: %v", err))
	} else {
		haproxyAPICallsSuccessful.WithLabelValues("set_server_state_ready").Inc()
		statsCountVec("haproxy_api_calls_successful_total", 1, "set_server_state_ready")
	}

	if len(errs) > 0 {
		return fmt.Errorf("haproxyUpdateExistingServer errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

func resolveWithIPFallback(hostname string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ip, err := resolveHostnameToIP(ctx, hostname)
	if err != nil {
		logger.WithFields(logrus.Fields{
			"hostname": hostname,
			"error":    err,
		}).Warning("DNS resolution failed, falling back to hostname")
		health.Lock()
		health.ResolverHealth.Healthy = false
		health.ResolverHealth.Message = fmt.Sprintf("DNS resolution failed for %s: %v", hostname, err)
		health.Unlock()
		return hostname, err
	}
	health.Lock()
	health.ResolverHealth.Healthy = true
	health.ResolverHealth.Message = "OK"
	health.Unlock()
	return ip, nil
}

func resolveHostnameToIP(ctx context.Context, hostname string) (string, error) {
	resolver := net.Resolver{}
	ips, err := resolver.LookupHost(ctx, hostname)
	if err != nil {
		return "", err
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("no IP addresses found for hostname: %s", hostname)
	}
	return ips[0], nil
}

func generateStableBackendName(app App, proxyPlatform string, groupName string) string {
	if proxyPlatform == "nginx" {
		return generateStableNginxUpstreamName(app, groupName)
	} else if proxyPlatform == "haproxy" {
		return generateStableHaproxyBackendName(app, groupName)
	}
	return app.Vhost
}

// generateStableNginxUpstreamName creates a valid upstream name from a vhost for nginx. Routing Tag is not supported yet for nginx+.
func generateStableNginxUpstreamName(app App, groupName string) string {
	vhost := app.Vhost
	return vhost
}

// generateStableHaproxyBackendName creates a valid and unique backend name from a vhost and optional routing tag.
func generateStableHaproxyBackendName(app App, groupName string) string {
	vhost := app.Vhost
	routingTagKey := app.RoutingTagKey
	routingTagValue := groupName

	// Only add suffix if the feature is enabled and a routing tag key is configured.
	if config.HaproxyBackendIncludeRoutingTagSuffix && routingTagKey != "" {
		return fmt.Sprintf("%s%s%s", vhost, config.HaproxyBackendNameSeparator, routingTagValue)
	}

	// Fallback to just the vhost if no routing tag is found or configured.
	return vhost
}

// generateStableHaproxyServerName creates a valid and unique server name from a host.
func generateStableHaproxyServerName(host Host) string {
	// Replace characters that are invalid in HAProxy server names.
	sanitizer := strings.NewReplacer(":", config.HaproxyServerNameHostPortSeparator)
	return fmt.Sprintf("%s%s%s%s%d", config.HaproxyServerNamePrefix, config.HaproxyServerNameHostPortSeparator, sanitizer.Replace(host.Host), config.HaproxyServerNameHostPortSeparator, host.Port)
}

func nginxPlus(data *RenderingData) error {
	//Current implementation only updates AppVhosts, does not suppport routing tag & LeaderVhost
	config.RLock()
	defer config.RUnlock()
	start := time.Now()
	var resultLabel string
	defer func() {
		duration := time.Since(start)
		nginxPlusReconcileAllBackendsDuration.WithLabelValues(resultLabel).Observe(duration.Seconds())
		statsTimingVec("nginxplus_runtime_api_duration", duration, resultLabel)
	}()

	logger.WithFields(logrus.Fields{
		"nginx": config.Nginxplusapiaddr,
	}).Debug("endpoint")

	endpoint := "http://" + config.Nginxplusapiaddr + "/api"
	//Create transport here for connection re-use
	tr := &http.Transport{
		MaxIdleConns:       30,
		DisableCompression: true,
	}

	client := &http.Client{Transport: tr}
	nginxClient, error := nplus.NewNginxClient(endpoint, nplus.WithHTTPClient(client), nplus.WithAPIVersion(
		8))
	if error != nil {
		logger.WithFields(logrus.Fields{
			"error": error,
		}).Error("unable to make call to nginx plus")
		updateHealthForUpstreamUpdateAPI(false, error.Error())
		return error
	}

	logger.WithFields(logrus.Fields{"apps": data.Apps}).Debug("Updating upstreams for the whitelisted http/s drove vhosts")
	reconciledApps := make(map[string]bool)
	reconciliationFailedApps := make(map[string]bool)
	for _, app := range data.Apps {
		//Ensure UpdateHTTPServers is not called for streams TCP/UDP instances
		isHTTPVHost := isHTTPHostGroup(app.Hosts)

		if isHTTPVHost && app.Vhost != "" && len(app.Hosts) > 0 {
			var newFormattedServers []string
			for _, t := range app.Hosts {
				if (string(t.PortType) == "http") || (string(t.PortType) == "https") {
					var hostAndPortMapping string
					ipRecord, error := resolveWithIPFallback(t.Host)
					if error != nil {
						logger.WithFields(logrus.Fields{
							"error":    error,
							"hostname": t.Host,
						}).Error("dns lookup failed !! skipping the hostname")
						reconciliationFailedApps[app.Vhost] = true
						updateHealthForUpstreamUpdateAPI(false, error.Error())
						continue
					}
					hostAndPortMapping = ipRecord + ":" + fmt.Sprint(t.Port)
					newFormattedServers = append(newFormattedServers, hostAndPortMapping)
				}

			}

			logger.WithFields(logrus.Fields{
				"vhost": app.Vhost,
			}).Debug("app.vhost")

			logger.WithFields(logrus.Fields{
				"upstreams": newFormattedServers,
			}).Debug("nginx upstreams")

			upstreamtocheck := app.Vhost
			var finalformattedServers []nplus.UpstreamServer

			for _, server := range newFormattedServers {
				formattedServer := nplus.UpstreamServer{Server: server, MaxFails: config.NginxMaxFailsUpstream, FailTimeout: config.NginxFailTimeoutUpstream, SlowStart: config.NginxSlowStartUpstream}
				finalformattedServers = append(finalformattedServers, formattedServer)
			}
			// If upstream has no servers, UpdateHTTPServers returns error as in-line GetHTTPServers returns error. server ID 0 needs to be explicitly initiated by a PATCH
			err := nginxClient.CheckIfUpstreamExists(upstreamtocheck)
			if err != nil {
				nginxAPICallsFailed.WithLabelValues("check_if_upstream_exists").Inc()
				statsCountVec("nginx_api_calls_failed_total", 1, "check_if_upstream_exists")
				// First add atleast one server to initialise upstream to support UpdateHTTPServers
				logger.WithFields(logrus.Fields{
					"Adding fresh upstream for": upstreamtocheck,
				}).Info("Adding first server for upstream")
				//Adding first server for server ID 0. ID 0 needs to be updated if state file is resurrected when a vhost gets resurrected. Create ID 0 otherwise.
				error := nginxClient.UpdateHTTPServer(upstreamtocheck, finalformattedServers[0])
				if error != nil {
					nginxAPICallsFailed.WithLabelValues("update_http_server").Inc()
					statsCountVec("nginx_api_calls_failed_total", 1, "update_http_server")
				} else {
					nginxAPICallsSuccessful.WithLabelValues("update_http_server").Inc()
					statsCountVec("nginx_api_calls_successful_total", 1, "update_http_server")
				}

				// Now upstream should have servers, update earlier state to let UpdateHTTPServers take over
				//But wait from some time for nginx to actually update it's state. Consecutive calls would still return a 404 if you don't wait long enough
				if error != nil {
					ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
					defer cancel()
					err = nginxClient.CheckIfUpstreamExists(upstreamtocheck)
				waitLoop:
					for err != nil {
						select {
						case <-ctx.Done():
							err = fmt.Errorf("context timeout waiting for upstream '%s' to exist", upstreamtocheck)
							logger.WithError(err).Error("Failed to confirm upstream creation")
							updateHealthForUpstreamUpdateAPI(false, err.Error())
							break waitLoop
						default:
							time.Sleep(5 * time.Millisecond)
							err = nginxClient.CheckIfUpstreamExists(upstreamtocheck)
						}
					}
					if err == nil {
						nginxAPICallsSuccessful.WithLabelValues("check_if_upstream_exists").Inc()
						statsCountVec("nginx_api_calls_successful_total", 1, "check_if_upstream_exists")
					}
				}

			}
			if err == nil {
				nginxAPICallsSuccessful.WithLabelValues("check_if_upstream_exists").Inc()
				statsCountVec("nginx_api_calls_successful_total", 1, "check_if_upstream_exists")
				added, deleted, updated, error := nginxClient.UpdateHTTPServers(upstreamtocheck, finalformattedServers)

				if error != nil {
					nginxAPICallsFailed.WithLabelValues("update_http_servers").Inc()
					statsCountVec("nginx_api_calls_failed_total", 1, "update_http_servers")
				} else {
					nginxAPICallsSuccessful.WithLabelValues("update_http_servers").Inc()
					statsCountVec("nginx_api_calls_successful_total", 1, "update_http_servers")
				}

				if added != nil {
					logger.WithFields(logrus.Fields{
						"vhost":           upstreamtocheck,
						"upstreams added": added,
					}).Info("nginx upstreams added")
				}
				if deleted != nil {
					logger.WithFields(logrus.Fields{
						"vhost":             upstreamtocheck,
						"upstreams deleted": deleted,
					}).Info("nginx upstreams deleted")
				}
				if updated != nil {
					logger.WithFields(logrus.Fields{
						"vhost":             upstreamtocheck,
						"upstreams updated": updated,
					}).Info("nginx upstreams updated")
				}
				if error != nil {
					logger.WithFields(logrus.Fields{
						"vhost": upstreamtocheck,
						"error": error,
					}).Error("unable to update nginx upstreams")
					return error
				}
			} else {
				reconciliationFailedApps[app.Vhost] = true
				logger.WithFields(logrus.Fields{
					"vhost": app.Vhost,
					"error": err,
				}).Error("unable to check if upstream exists in nginx plus")
				updateHealthForUpstreamUpdateAPI(false, err.Error())
				return err
			}
		} else {
			logger.WithFields(logrus.Fields{"vhost": app.Vhost}).Debug("Skipping non-HTTP/S vhost update")
		}
		reconciledApps[app.Vhost] = true
	}
	if len(reconciliationFailedApps) > 0 {
		resultLabel = "error"
		logger.WithField("failed_apps", reconciliationFailedApps).Error("Failed to reconcile some nginx plus vhosts")
		updateHealthForUpstreamUpdateAPI(false, errors.New("failed to reconcile some nginx plus vhosts: "+fmt.Sprintf("%v", reconciliationFailedApps)).Error())
	}
	if len(reconciledApps) == 0 {
		resultLabel = "error"
		updateHealthForUpstreamUpdateAPI(false, errors.New("failed to reconcile any nginx plus vhosts").Error())
		return errors.New("failed to reconcile any nginx plus vhosts")
	}
	resultLabel = "success"
	logger.Info("Successfully reconciled all nginx plus vhosts")
	updateHealthForUpstreamUpdateAPI(true, "OK")
	return nil
}

var tmplCache *template.Template
var tmplCacheErr error
var tmplCacheOnce sync.Once

func getTmpl(proxyTemplatePath string) (*template.Template, error) {
	tmplCacheOnce.Do(func() {
		logger.WithFields(logrus.Fields{
			"file": proxyTemplatePath,
		}).Info("Reading template")
		tmplCache, tmplCacheErr = template.New(filepath.Base(proxyTemplatePath)).
			Delims(config.LeftDelimiter, config.RightDelimiter).
			Funcs(template.FuncMap{
				"hasPrefix": strings.HasPrefix,
				"hasSuffix": strings.HasPrefix,
				"contains":  strings.Contains,
				"split":     strings.Split,
				"join":      strings.Join,
				"trim":      strings.Trim,
				"replace":   strings.Replace,
				"tolower":   strings.ToLower,
				"getenv":    os.Getenv,
				"datetime":  time.Now,
			}).
			ParseFiles(proxyTemplatePath)
		if tmplCacheErr != nil {
			logger.WithFields(logrus.Fields{
				"error": tmplCacheErr,
				"file":  proxyTemplatePath,
			}).Error("unable to read template")
			health.Lock()
			health.Template.Healthy = false
			health.Template.Message = tmplCacheErr.Error()
			health.Unlock()
		} else {
			logger.WithFields(logrus.Fields{
				"file": proxyTemplatePath,
			}).Info("Template read successfully")
			health.Lock()
			health.Template.Healthy = true
			health.Template.Message = "OK"
			health.Unlock()
		}
	})
	return tmplCache, tmplCacheErr
}

func checkConf(path string) error {
	// Always return OK if disabled in config.
	if IgnoreCheck {
		return nil
	}
	// This is to allow arguments as well. Example "docker exec nginx..."
	args := strings.Fields(ProgramCmd)
	head := args[0]
	args = args[1:]
	args = append(args, ProgramCmdConfFileArg)
	args = append(args, path)
	args = append(args, ProgramCmdConfTestArg)
	cmd := exec.Command(head, args...)
	//e.g for nginx cmd := exec.Command(parts..., "-c", path, "-t")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run() // will wait for command to return
	if err != nil {
		msg := fmt.Sprint(err) + ": " + stderr.String()
		errstd := errors.New(msg)
		return errstd
	}
	return nil
}

func reloadNginx() error {
	logger.Info("Reloading nginx with cmd: " + config.NginxCmd)
	// This is to allow arguments as well. Example "docker exec nginx..."
	args := strings.Fields(config.NginxCmd)
	head := args[0]
	args = args[1:]
	args = append(args, "-s")
	args = append(args, "reload")
	cmd := exec.Command(head, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run() // will wait for command to return
	if err != nil {
		msg := fmt.Sprint(err) + ": " + stderr.String()
		errstd := errors.New(msg)
		return errstd
	}
	return nil
}

func reloadHaproxy() error {
	logger.Info("Reloading haproxy with cmd: " + config.HaproxyReloadCmd)
	// This is to allow other cmds as well. Example "docker exec haproxy..." or SIGUSR2 to master worker
	args := strings.Fields(config.HaproxyReloadCmd)
	head := args[0]
	args = args[1:]
	cmd := exec.Command(head, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run() // will wait for command to return
	if err != nil {
		msg := fmt.Sprint(err) + ": " + stderr.String()
		errstd := errors.New(msg)
		return errstd
	}
	return nil
}

func reloadWorker() {
	go func() {
		// a ticker channel to limit reloads to drove, 1s is enough for now.
		ticker := time.NewTicker(1 * time.Second)
		for {
			select {
			case <-ticker.C:
				<-appsConfigUpdateSignalQueue
				reload()
			}
		}
	}()
}
