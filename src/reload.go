package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	Xproxy              string
	LeftDelimiter       string                            `json:"-" toml:"left_delimiter"`
	RightDelimiter      string                            `json:"-" toml:"right_delimiter"`
	MaxFailsUpstream    *int                              `json:"max_fails,omitempty"`
	FailTimeoutUpstream string                            `json:"fail_timeout,omitempty"`
	SlowStartUpstream   string                            `json:"slow_start,omitempty"`
	Namespaces          map[string]NamespaceRenderingData `json:"namespaces"`
	Apps                map[string]App
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
		if (ConfigReloadDisabled) && config.ProxyPlatform == "nginx" {
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
		health.Lock()
		if err != nil {
			health.UpstreamUpdatesViaAPI.Healthy = false
			health.UpstreamUpdatesViaAPI.Message = err.Error()
			logger.WithFields(logrus.Fields{
				"error": err.Error(),
			}).Error("unable to update upstreams via " + config.ProxyPlatform + " api")
			go statsCount("reload.failed", 1)
			go countFailedReloads.Inc()
		} else {
			health.UpstreamUpdatesViaAPI.Healthy = true
			health.UpstreamUpdatesViaAPI.Message = "OK"
		}
		health.Unlock()
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
	data.LeftDelimiter = staticData.LeftDelimiter
	data.RightDelimiter = staticData.RightDelimiter
	data.FailTimeoutUpstream = staticData.FailTimeoutUpstream
	data.MaxFailsUpstream = staticData.MaxFailsUpstream
	data.SlowStartUpstream = staticData.SlowStartUpstream
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
	lastConfig = tmpFile.Name()
	err = template.Execute(tmpFile, &data)
	if err != nil {
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

var (
	haproxyClient     runtime_api.Runtime
	haproxyClientOnce sync.Once
	haproxyClientErr  error
)

func getHAProxyClient(ctx context.Context) (runtime_api.Runtime, error) {
	haproxyClientOnce.Do(func() {
		haproxyClient, haproxyClientErr = newHAProxyClient(ctx)
	})
	return haproxyClient, haproxyClientErr
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
	config.RLock()
	defer config.RUnlock()

	runtimeClient, err := getHAProxyClient(ctx)
	if err != nil {
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
			} else if !isHTTPHostGroup(groupData.Hosts) {
				logger.WithFields(logrus.Fields{
					"Vhost": app.Vhost,
					"group": groupName,
					"hosts": groupData.Hosts,
				}).Warning("Non-HTTP/HTTPS host group detected. Skipping HAProxy runtime API update for this group.")
			} else {
				backendName := generateStableHaproxyBackendName(app, groupName)
				if backendName != "" {
					backendsToReconcile[backendName] = append(backendsToReconcile[backendName], groupData.Hosts...)
				}
			}
		}
	}

	// Reconcile each unique backend. GetServersState can only be called once per backend. Runtime API library doesn't return backend names
	// when calling GetServersState as models.RuntimeServers is a list of []*RuntimeServer without backend context returned by models.ParseRuntimeServer()
	logger.WithField("count", len(backendsToReconcile)).Debug("Reconciling all unique HAProxy backends")
	for backend, hosts := range backendsToReconcile {
		// Fetch servers for the specific backend.
		currentServersForBackend, err := runtimeClient.GetServersState(backend)
		if err != nil {
			// This error is often not fatal; it can mean the backend doesn't exist yet.
			logger.WithFields(logrus.Fields{
				"backend": backend,
				"error":   err,
			}).Warning("Could not get servers for backend, it may be new. Proceeding with reconciliation.")
			// Pass an empty slice so reconciliation can proceed to add servers.
			currentServersForBackend = []*runtime_models.RuntimeServer{}
		} else {
			haproxyAPICallsSuccessful.WithLabelValues("get_servers_state").Inc()
			statsCountVec("haproxy_api_calls_successful_total", 1, "get_servers_state")
		}

		if err := reconcileHAProxyBackend(runtimeClient, backend, hosts, currentServersForBackend); err != nil {
			logger.WithFields(logrus.Fields{
				"backend": backend,
				"error":   err,
			}).Error("Failed to reconcile HAProxy backend")
			// Continue with other backends instead of failing completely
		}
	}
	logger.Info("Successfully reconciled all HAProxy backends")

	return nil
}

func reconcileHAProxyBackend(client runtime_api.Runtime, backend string, desiredHosts []Host, currentServers []*runtime_models.RuntimeServer) error {
	logger.WithField("backend", backend).Debug("Reconciling HAProxy backend")

	desiredServerMap := haproxyBuildDesiredServerMap(desiredHosts)
	currentServerMap := haproxyBuildCurrentServerMap(currentServers)

	if haproxyAreServerMapsIdentical(desiredServerMap, currentServerMap) {
		logger.WithField("backend", backend).Debug("HAProxy backend is already in the desired state. No changes needed.")
		return nil
	}

	haproxyAddOrUpdateServers(client, backend, desiredServerMap, currentServerMap)
	//add or update servers first to avoid downtime in case of complete replacement of servers
	haproxyRemoveStaleServers(client, backend, currentServers, desiredServerMap)

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
		desiredIP := desiredHost.Host // Default to hostname if resolution fails.
		resolveCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if resolvedIP, err := resolveHostnameToIP(resolveCtx, desiredHost.Host); err == nil {
			desiredIP = resolvedIP
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

func haproxyRemoveStaleServers(client runtime_api.Runtime, backend string, currentServers []*runtime_models.RuntimeServer, desiredServerMap map[string]Host) {
	for _, srv := range currentServers {
		if _, exists := desiredServerMap[srv.Name]; !exists {
			logger.WithFields(logrus.Fields{"backend": backend, "server": srv.Name}).Info("Disabling and deleting stale server")
			// Set server state to 'maint' before deleting.
			if err := client.SetServerState(backend, srv.Name, "maint"); err != nil {
				haproxyAPICallsFailed.WithLabelValues("set_server_state_maint").Inc()
				statsCountVec("haproxy_api_calls_failed_total", 1, "set_server_state_maint")
				logger.WithFields(logrus.Fields{"backend": backend, "server": srv.Name, "error": err}).Error("Failed to disable server")
			} else {
				haproxyAPICallsSuccessful.WithLabelValues("set_server_state_maint").Inc()
				statsCountVec("haproxy_api_calls_successful_total", 1, "set_server_state_maint")
			}

			if err := client.DeleteServer(backend, srv.Name); err != nil {
				haproxyAPICallsFailed.WithLabelValues("delete_server").Inc()
				statsCountVec("haproxy_api_calls_failed_total", 1, "delete_server")
				logger.WithFields(logrus.Fields{"backend": backend, "server": srv.Name, "error": err}).Error("Failed to delete server")
			} else {
				haproxyAPICallsSuccessful.WithLabelValues("delete_server").Inc()
				statsCountVec("haproxy_api_calls_successful_total", 1, "delete_server")
			}
		}
	}
}

func haproxyAddOrUpdateServers(client runtime_api.Runtime, backend string, desiredServerMap map[string]Host, currentServerMap map[string]runtime_models.RuntimeServer) {
	for serverName, host := range desiredServerMap {
		if _, exists := currentServerMap[serverName]; !exists {
			haproxyAddNewServer(client, backend, serverName, host)
		} else {
			haproxyUpdateExistingServer(client, backend, serverName, host, currentServerMap[serverName])
		}
	}
}

func haproxyAddNewServer(client runtime_api.Runtime, backend, serverName string, host Host) {
	logger.WithFields(logrus.Fields{"backend": backend, "server": serverName}).Info("Adding new server")
	if err := client.AddServer(backend, serverName, fmt.Sprintf("%s:%d", host.Host, host.Port)); err != nil {
		haproxyAPICallsFailed.WithLabelValues("add_server").Inc()
		statsCountVec("haproxy_api_calls_failed_total", 1, "add_server")
		logger.WithFields(logrus.Fields{"backend": backend, "server": serverName, "error": err}).Error("Failed to add server")
		return
	}
	haproxyAPICallsSuccessful.WithLabelValues("add_server").Inc()
	statsCountVec("haproxy_api_calls_successful_total", 1, "add_server")

	if err := client.SetServerState(backend, serverName, "ready"); err != nil {
		haproxyAPICallsFailed.WithLabelValues("set_server_state_ready").Inc()
		statsCountVec("haproxy_api_calls_failed_total", 1, "set_server_state_ready")
		logger.WithFields(logrus.Fields{"backend": backend, "server": serverName, "error": err}).Error("Failed to set new server state to ready")
	} else {
		haproxyAPICallsSuccessful.WithLabelValues("set_server_state_ready").Inc()
		statsCountVec("haproxy_api_calls_successful_total", 1, "set_server_state_ready")
	}
}

func haproxyUpdateExistingServer(client runtime_api.Runtime, backend, serverName string, host Host, currentServer runtime_models.RuntimeServer) {
	// Resolve desired hostname to IP for a comparison with HAProxy's runtime state.
	desiredIP := host.Host // Default to hostname if resolution fails. This will help a force update even if resolution is unavailable.
	resolveCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if resolvedIP, err := resolveHostnameToIP(resolveCtx, host.Host); err != nil {
		logger.WithFields(logrus.Fields{"hostname": host.Host, "error": err}).Warning("Failed to resolve hostname; using hostname for comparison")
	} else {
		desiredIP = resolvedIP
	}

	// Check if the server is already in the desired state.
	portMatches := currentServer.Port != nil && *currentServer.Port == int64(host.Port)
	if currentServer.Address == desiredIP && portMatches && currentServer.AdminState == "ready" && currentServer.OperationalState == "up" {
		logger.WithFields(logrus.Fields{"backend": backend, "server": serverName}).Trace("Server is up-to-date, no update needed")
		return
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

	if err := client.SetServerAddr(backend, serverName, desiredIP, int(host.Port)); err != nil {
		haproxyAPICallsFailed.WithLabelValues("set_server_addr").Inc()
		statsCountVec("haproxy_api_calls_failed_total", 1, "set_server_addr")
		logger.WithFields(logrus.Fields{"backend": backend, "server": serverName, "error": err}).Error("Failed to set server address")
	} else {
		haproxyAPICallsSuccessful.WithLabelValues("set_server_addr").Inc()
		statsCountVec("haproxy_api_calls_successful_total", 1, "set_server_addr")
	}

	if err := client.SetServerState(backend, serverName, "ready"); err != nil {
		haproxyAPICallsFailed.WithLabelValues("set_server_state_ready").Inc()
		statsCountVec("haproxy_api_calls_failed_total", 1, "set_server_state_ready")
		logger.WithFields(logrus.Fields{"backend": backend, "server": serverName, "error": err}).Error("Failed to set server state to ready")
	} else {
		haproxyAPICallsSuccessful.WithLabelValues("set_server_state_ready").Inc()
		statsCountVec("haproxy_api_calls_successful_total", 1, "set_server_state_ready")
	}
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
	return fmt.Sprintf("%s_%s_%d", config.HaproxyServerNamePrefix, sanitizer.Replace(host.Host), host.Port)
}

func nginxPlus(data *RenderingData) error {
	//Current implementation only updates AppVhosts, does not suppport routing tag & LeaderVhost
	config.RLock()
	defer config.RUnlock()

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
		return error
	}

	logger.WithFields(logrus.Fields{"apps": data.Apps}).Debug("Updating upstreams for the whitelisted http/s drove vhosts")
	for _, app := range data.Apps {
		//Ensure UpdateHTTPServers is not called for streams TCP/UDP instances
		isHTTPVHost := isHTTPHostGroup(app.Hosts)

		if isHTTPVHost && app.Vhost != "" && len(app.Hosts) > 0 {
			var newFormattedServers []string
			for _, t := range app.Hosts {
				if (string(t.PortType) == "http") || (string(t.PortType) == "https") {
					var hostAndPortMapping string
					ipRecords, error := net.LookupHost(string(t.Host))
					if error != nil {
						logger.WithFields(logrus.Fields{
							"error":    error,
							"hostname": t.Host,
						}).Error("dns lookup failed !! skipping the hostname")
						continue
					}
					ipRecord := ipRecords[0]
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
				formattedServer := nplus.UpstreamServer{Server: server, MaxFails: config.MaxFailsUpstream, FailTimeout: config.FailTimeoutUpstream, SlowStart: config.SlowStartUpstream}
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
					for err != nil {
						select {
						case <-ctx.Done():
							logger.WithFields(logrus.Fields{
								"Adding fresh upstream for": upstreamtocheck,
							}).Error("Context timeout waiting for CheckIfUpstreamExists")
						default:
							time.Sleep(5 * time.Millisecond)
							err = nginxClient.CheckIfUpstreamExists(upstreamtocheck)
						}
					}
					if err == nil {
						nginxAPICallsSuccessful.WithLabelValues("check_if_upstream_exists").Inc()
						statsCountVec("nginx_api_calls_successful_total", 1, "check_if_upstream_exists")
					}
					cancel()
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
				return err
			}
		} else {
			logger.WithFields(logrus.Fields{"vhost": app.Vhost}).Debug("Skipping non-HTTP/S vhost update")
		}
	}
	return nil
}

func checkTmpl() error {
	config.RLock()
	defer config.RUnlock()
	data := RenderingData{}
	createRenderingData(&data)
	t, err := getTmpl(TemplatePath)
	health.Lock()
	if err != nil {
		health.Template.Healthy = false
		health.Template.Message = err.Error()
	} else {
		err = t.Execute(io.Discard, &data)
		if err != nil {
			health.Template.Healthy = false
			health.Template.Message = err.Error()
		} else {
			health.Template.Healthy = true
			health.Template.Message = "OK"
		}
	}
	health.Unlock()
	return err
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
		} else {
			logger.WithFields(logrus.Fields{
				"file": proxyTemplatePath,
			}).Info("Template read successfully")
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
