package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	runtime_misc "github.com/haproxytech/client-native/v5/misc"
	runtime_models "github.com/haproxytech/client-native/v5/models"
	runtime_api "github.com/haproxytech/client-native/v5/runtime"
	runtime_options "github.com/haproxytech/client-native/v5/runtime/options"
)

type HaproxyManager struct {
	client runtime_api.Runtime
}

func NewHaproxyManager(ctx context.Context, haproxySocketAddr string, disableLargeBackendCountOptimisation bool) (*HaproxyManager, error) {
	logger.WithField("haproxy_socket", haproxySocketAddr).Debug("Preparing to connect to HAProxy runtime API")

	if haproxySocketAddr == "" {
		return nil, errors.New("HAProxy socket address is not configured")
	}

	if IsUnixSocketAddr(haproxySocketAddr) {
		if _, err := os.Stat(haproxySocketAddr); os.IsNotExist(err) {
			return nil, fmt.Errorf("HAProxy socket file does not exist: %s", haproxySocketAddr)
		}
	}

	runtimeClient, err := runtime_api.New(ctx, runtime_options.Sockets(map[int]string{1: haproxySocketAddr}))
	if err != nil {
		return nil, fmt.Errorf("error initializing HAProxy runtime client: %w", err)
	}

	if _, err := runtimeClient.GetInfo(); err != nil {
		return nil, fmt.Errorf("error connecting to HAProxy socket: %w", err)
	}

	logger.Info("Successfully connected to HAProxy runtime API")
	return &HaproxyManager{client: runtimeClient}, nil
}

func (m *HaproxyManager) ReconcileAllBackends(data *RenderingData, disableLargeBackendCountOptimisation bool) error {
	start := time.Now()
	var resultLabel string
	defer func() {
		duration := time.Since(start)
		haproxyReconcileAllBackendsDuration.WithLabelValues(resultLabel).Observe(duration.Seconds())
	}()

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
	err := error(nil)
	if !config.HaproxyDisableLargeBackendCountOptimisation {
		logger.Debug("HAProxy large backend count optimisation is enabled. Using aggregated call to get all backends and servers state")
		allServersState, err = m.getServersStateWithBackend()
		if err != nil {
			resultLabel = "error"
			updateHealthForUpstreamUpdateAPI(false, err.Error())
			logger.WithFields(logrus.Fields{
				"error": err,
			}).Error("Failed to get HAProxy servers state for all backends")
			haproxyAPICallsFailed.WithLabelValues("get_servers_state_all_backends").Inc()
			return fmt.Errorf("failed to get HAProxy servers state for all backends: %w", err)
		}
		haproxyAPICallsSuccessful.WithLabelValues("get_servers_state_all_backends").Inc()
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
			currentServersForBackend, err = m.client.GetServersState(backend)
		}

		if err != nil {
			reconciliationFailedBackends[backend] = true
			// This error is often not fatal; it can mean the backend doesn't exist yet.
			logger.WithFields(logrus.Fields{
				"backend": backend,
				"error":   err,
			}).Warning("Could not get servers for backend, it may be new. Proceeding with reconciliation.")
			haproxyAPICallsFailed.WithLabelValues("get_servers_state").Inc()
			// Pass an empty slice so reconciliation can proceed to add servers.
			currentServersForBackend = []*runtime_models.RuntimeServer{}
		} else {
			haproxyAPICallsSuccessful.WithLabelValues("get_servers_state").Inc()
		}

		if err := m.reconcileBackend(backend, hosts, currentServersForBackend); err != nil {
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

func (m *HaproxyManager) reconcileBackend(backend string, desiredHosts []Host, currentServers []*runtime_models.RuntimeServer) error {
	var errs []string
	logger.WithField("backend", backend).Debug("Reconciling HAProxy backend")

	desiredServerMap := m.buildDesiredServerMap(desiredHosts)
	currentServerMap := m.buildCurrentServerMap(currentServers)

	if m.areServerMapsIdentical(desiredServerMap, currentServerMap) {
		logger.WithField("backend", backend).Debug("HAProxy backend is already in the desired state. No changes needed.")
		return nil
	}

	if err := m.addOrUpdateServers(backend, desiredServerMap, currentServerMap); err != nil {
		errs = append(errs, fmt.Sprintf("add/update servers: %v", err))
	}
	//add or update servers first to avoid downtime in case of complete replacement of servers
	if err := m.removeStaleServers(backend, currentServers, desiredServerMap); err != nil {
		errs = append(errs, fmt.Sprintf("remove stale servers: %v", err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to reconcile HAProxy backend: %v", errs)
	}

	logger.WithField("backend", backend).Info("Successfully reconciled HAProxy backend")
	return nil
}

func (m *HaproxyManager) areServerMapsIdentical(desiredMap map[string]Host, currentMap map[string]runtime_models.RuntimeServer) bool {
	if len(desiredMap) != len(currentMap) {
		return false
	}
	for serverName, desiredHost := range desiredMap {
		currentServer, exists := currentMap[serverName]
		if !exists {
			return false
		}
		match, _, _ := m.compareServerConfigs(desiredHost, currentServer)
		if !match {
			return false
		}
	}
	return true
}

func (m *HaproxyManager) compareServerConfigs(desiredHost Host, current runtime_models.RuntimeServer) (bool, string, string) {
	// Resolve desired hostname to IP for a reliable comparison with HAProxy's runtime state.
	desiredIP, err := resolveWithIPFallback(desiredHost.Host)
	if err != nil {
		logger.WithFields(logrus.Fields{"hostname": desiredHost.Host, "error": err}).Warning("Failed to resolve hostname; using hostname for comparison")
	}
	resolveCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if resolvedIP, err := resolveHostnameToIP(resolveCtx, desiredHost.Host); err == nil {
		desiredIP = resolvedIP
	}
	portMatches := current.Port != nil && *current.Port == int64(desiredHost.Port)
	addressMatches := current.Address == desiredIP
	adminStateMatches := current.AdminState == "ready"
	// We only check for 'up' or 'maint' as operational state can fluctuate (e.g., 'going down').
	// A server in 'maint' is operationally down but administratively configured, so we consider it a match if we want it 'up'.
	opStateMatches := current.OperationalState == "up" || current.OperationalState == "maint"
	serverConfigsMatch := portMatches && addressMatches && adminStateMatches && opStateMatches
	return serverConfigsMatch, desiredIP, current.Address
}

func (m *HaproxyManager) buildDesiredServerMap(desiredHosts []Host) map[string]Host {
	serverMap := make(map[string]Host)
	for _, host := range desiredHosts {
		serverName := generateStableHaproxyServerName(host)
		serverMap[serverName] = host
	}
	return serverMap
}

func (m *HaproxyManager) buildCurrentServerMap(currentServers []*runtime_models.RuntimeServer) map[string]runtime_models.RuntimeServer {
	serverMap := make(map[string]runtime_models.RuntimeServer)
	for _, srv := range currentServers {
		serverMap[srv.Name] = *srv
	}
	return serverMap
}

func (m *HaproxyManager) removeStaleServers(backend string, currentServers []*runtime_models.RuntimeServer, desiredServerMap map[string]Host) error {
	var errs []string
	for _, srv := range currentServers {
		if _, exists := desiredServerMap[srv.Name]; !exists {
			logger.WithFields(logrus.Fields{"backend": backend, "server": srv.Name}).Info("Disabling and deleting stale server")
			if err := m.client.SetServerState(backend, srv.Name, "maint"); err != nil {
				haproxyAPICallsFailed.WithLabelValues("set_server_state_maint").Inc()
				logger.WithFields(logrus.Fields{"backend": backend, "server": srv.Name, "error": err}).Error("Failed to disable server")
				errs = append(errs, fmt.Sprintf("disable %s: %v", srv.Name, err))
			} else {
				haproxyAPICallsSuccessful.WithLabelValues("set_server_state_maint").Inc()
			}
			if err := m.client.DeleteServer(backend, srv.Name); err != nil {
				haproxyAPICallsFailed.WithLabelValues("delete_server").Inc()
				logger.WithFields(logrus.Fields{"backend": backend, "server": srv.Name, "error": err}).Error("Failed to delete server")
				errs = append(errs, fmt.Sprintf("delete %s: %v", srv.Name, err))
			} else {
				haproxyAPICallsSuccessful.WithLabelValues("delete_server").Inc()
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("errors disabling stale servers: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (m *HaproxyManager) addOrUpdateServers(backend string, desiredServerMap map[string]Host, currentServerMap map[string]runtime_models.RuntimeServer) error {
	var errs []string
	for serverName, host := range desiredServerMap {
		if _, exists := currentServerMap[serverName]; !exists {
			if err := m.addNewServer(backend, serverName, host); err != nil {
				errs = append(errs, fmt.Sprintf("add %s: %v", serverName, err))
			}
		} else {
			if err := m.updateExistingServer(backend, serverName, host, currentServerMap[serverName]); err != nil {
				errs = append(errs, fmt.Sprintf("update %s: %v", serverName, err))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("errors in Add/Update servers: %s", strings.Join(errs, "; "))
	}
	return nil
}

// settings from default-server statement in config are not applied when adding server via runtime api
// all default-server params should be added explictly here
// only use IP while adding server, not fqdn. HAProxy resolves the hostname only at startup/reload and dynamic servers don't support fqdn resolution
func (m *HaproxyManager) addNewServer(backend, serverName string, host Host) error {
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

	if err := m.client.AddServer(backend, serverName, fmt.Sprintf("%s:%d %s", desiredIP, host.Port, attributes_string)); err != nil {
		haproxyAPICallsFailed.WithLabelValues("add_server").Inc()
		logger.WithFields(logrus.Fields{"backend": backend, "server": serverName, "error": err}).Error("Failed to add server")
		return err
	}
	haproxyAPICallsSuccessful.WithLabelValues("add_server").Inc()

	if err := m.client.SetServerState(backend, serverName, "ready"); err != nil {
		haproxyAPICallsFailed.WithLabelValues("set_server_state_ready").Inc()
		logger.WithFields(logrus.Fields{"backend": backend, "server": serverName, "error": err}).Error("Failed to set new server state to ready")
		return err
	}
	haproxyAPICallsSuccessful.WithLabelValues("set_server_state_ready").Inc()
	return nil
}

func (m *HaproxyManager) updateExistingServer(backend, serverName string, host Host, currentServer runtime_models.RuntimeServer) error {
	match, desiredIP, _ := m.compareServerConfigs(host, currentServer)
	if match {
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

	if err := m.client.SetServerAddr(backend, serverName, desiredIP, int(host.Port)); err != nil {
		haproxyAPICallsFailed.WithLabelValues("set_server_addr").Inc()
		logger.WithFields(logrus.Fields{"backend": backend, "server": serverName, "error": err}).Error("Failed to set server address")
		errs = append(errs, fmt.Sprintf("set address: %v", err))
	} else {
		haproxyAPICallsSuccessful.WithLabelValues("set_server_addr").Inc()
	}

	if err := m.client.SetServerHealth(backend, serverName, "up"); err != nil {
		haproxyAPICallsFailed.WithLabelValues("set_server_health_up").Inc()
		logger.WithFields(logrus.Fields{"backend": backend, "server": serverName, "error": err}).Error("Failed to set server health to up")
		errs = append(errs, fmt.Sprintf("set health: %v", err))
	} else {
		haproxyAPICallsSuccessful.WithLabelValues("set_server_health_up").Inc()
	}

	if err := m.client.SetServerState(backend, serverName, "ready"); err != nil {
		haproxyAPICallsFailed.WithLabelValues("set_server_state_ready").Inc()
		logger.WithFields(logrus.Fields{"backend": backend, "server": serverName, "error": err}).Error("Failed to set server state to ready")
		errs = append(errs, fmt.Sprintf("set state: %v", err))
	} else {
		haproxyAPICallsSuccessful.WithLabelValues("set_server_state_ready").Inc()
	}

	if len(errs) > 0 {
		return fmt.Errorf("haproxyUpdateExistingServer errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Start of custom functions not available in HAProxy runtime client library
// getServersStateWithBackend calls "show servers state" command and parses the output to get servers grouped by backend name

func (m *HaproxyManager) getServersStateWithBackend() (map[string]runtime_models.RuntimeServers, error) {
	cmd := "show servers state"
	result, err := m.executeWithResponse(cmd)
	if err != nil {
		return nil, err
	}
	return m.parseRuntimeServersWithBackend(result)
}

func (m *HaproxyManager) executeWithResponse(command string) (string, error) {
	rawdata, err := m.client.ExecuteRaw(command)
	if err != nil {
		return "", fmt.Errorf("%w [%s]", err, command)
	}
	output := strings.Join(rawdata, "\n")
	if len(output) > 4 {
		switch output[0:4] {
		case "[3]:", "[2]:", "[1]:", "[0]:":
			return "", fmt.Errorf("[%c] %s [%s]", output[1], output[4:], command)
		}
	}
	return output, nil
}

func (m *HaproxyManager) parseRuntimeServersWithBackend(output string) (map[string]runtime_models.RuntimeServers, error) {
	lines := strings.Split(output, "\n")
	result := make(map[string]runtime_models.RuntimeServers)

	if strings.TrimSpace(lines[0]) != "1" {
		return nil, fmt.Errorf("unsupported output format version, supporting format version 1")
	}
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "1" {
			continue
		}
		backend, server := m.parseRuntimeServerWithBackend(line)
		if server != nil && backend != "" {
			if _, ok := result[backend]; !ok {
				result[backend] = []*runtime_models.RuntimeServer{}
			}
			result[backend] = append(result[backend], server)
		}
	}
	return result, nil
}

func (m *HaproxyManager) parseRuntimeServerWithBackend(line string) (backend string, server *runtime_models.RuntimeServer) {
	fields := strings.Split(line, " ")

	if len(fields) < 19 {
		return "", nil
	}

	p, err := strconv.ParseInt(fields[18], 10, 64)
	var port *int64
	if err == nil {
		port = &p
	}

	admState, _ := runtime_misc.GetServerAdminState(fields[6])

	var opState string
	switch fields[5] {
	case "0":
		opState = "down"
	case "3":
		opState = "stopping"
	case "1", "2":
		opState = "up"
	}

	return fields[1], &runtime_models.RuntimeServer{
		Name:             fields[3],
		Address:          fields[4],
		Port:             port,
		ID:               fields[2],
		AdminState:       admState,
		OperationalState: opState,
	}
}
