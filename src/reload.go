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
	"text/template"
	"time"

	nplus "github.com/nginxinc/nginx-plus-go-client/client"
	"github.com/sirupsen/logrus"

	runtime_api "github.com/haproxytech/client-native/v6/runtime"
	runtime_options "github.com/haproxytech/client-native/v6/runtime/options"
)

type NamespaceRenderingData struct {
	LeaderVHost string
	Leader      LeaderController
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
	}

	if !upstreamUpdateAPIEnabled {
		logger.Debug("Runtime API calls to update upstreams are disabled")
		//Any use of runtime API is disabled
		err = updateAndReloadConfig(&data, false)
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
		//For HAProxy, config is generated but not loaded even when reload is disabled as there is not other way to persist state across reloads
		//For Nginx+, ngx http_api maintains it's own state files if referenced in the running nginx config. Hence no templating is done at all when reload is disabled
		if (ConfigReloadDisabled) && config.ProxyPlatform == "nginx" {
			logger.Debug("Nginx: Template reload has been disabled")
		} else {
			vhosts := db.ReadAllKnownVhosts()
			lastKnownVhosts := db.ReadLastKnownVhosts()
			if !reflect.DeepEqual(vhosts, lastKnownVhosts) {
				logger.Info("Vhost changes detected. Need to reload config")
				err = updateAndReloadConfig(&data, false)
				if err != nil {
					logger.WithFields(logrus.Fields{
						"error": err.Error(),
					}).Error("unable to update and reload " + config.ProxyPlatform + " config. Runtime api calls to update upstreams will be skipped.")
					return err
				}
			} else {
				logger.Debug("No changes detected in vhosts. No config update is necessary. Upstream updates will happen via " + config.ProxyPlatform + " apis")
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

func updateAndReloadConfig(data *RenderingData, reloadDisabled bool) error {
	logger.Debug("Updating config with reload")
	start := time.Now()
	config.LastUpdates.LastSync = time.Now()
	vhosts := db.ReadAllKnownVhosts()
	err := writeConf(data)
	if err != nil {
		logger.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Error("unable to generate nginx config")
		go statsCount("reload.failed", 1)
		go countFailedReloads.Inc()
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
			logger.WithFields(logrus.Fields{
				"took": elapsed,
			}).Debug("config updated and reloaded successfully")
			go statsCount("reload.success", 1)
			go statsTiming("reload.time", elapsed)
			go countSuccessfulReloads.Inc()
			go observeReloadTimeMetric(elapsed)
			config.LastUpdates.LastProxyProgramReload = time.Now()
			db.UpdateLastKnownVhosts(vhosts)
		}
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
		}
		//Merging App if already exists
		for appId, appData := range nmData.Apps {
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
	}).Debug("Rendering data generated")
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
	if err != nil {
		logger.Error("Error in config generated")
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

func haproxyRuntimeAPI(data *RenderingData, ctx context.Context) error {
	//Current implementation only updates AppVhosts, does not support routing tag & LeaderVhost
	config.RLock()
	defer config.RUnlock()

	haproxySocket := config.HaproxySocketAddr
	logger.WithFields(logrus.Fields{
		"haproxy_socket": haproxySocket,
	}).Debug("Preparing to connect to HAProxy runtime API")

	// 1. Validate that the socket path is configured.
	if haproxySocket == "" {
		return errors.New("HAProxy socket address is not configured")
	}

	// 2. For Unix sockets, ensure the socket file exists before attempting to connect.
	if IsUnixSocketAddr(haproxySocket) {
		if _, err := os.Stat(haproxySocket); os.IsNotExist(err) {
			return fmt.Errorf("HAProxy socket file does not exist: %s", haproxySocket)
		}
	}
	// 3. Initialize the runtime client with context and configuration options.
	runtimeClient, err := runtime_api.New(ctx,
		runtime_options.MasterSocket(haproxySocket),
	)
	if err != nil {
		return fmt.Errorf("error initializing HAProxy runtime client: %w", err)
	}

	// 4. Test the connection to ensure the runtime API is responsive.
	if _, err := runtimeClient.ExecuteRaw("show info"); err != nil {
		return fmt.Errorf("error connecting to HAProxy socket, check if HAProxy is running and socket is correct: %w", err)
	}
	logger.Debug("Successfully connected to HAProxy runtime API")

	// Update upstreams for each app
	for appID, app := range data.Apps {
		if app.Vhost == "" {
			logger.WithFields(logrus.Fields{
				"app_id": appID,
			}).Debug("Skipping app without vhost")
			continue
		}

		err := updateHAProxyUpstream(runtimeClient, app)
		if err != nil {
			logger.WithFields(logrus.Fields{
				"app_id": appID,
				"vhost":  app.Vhost,
				"error":  err,
			}).Error("Failed to update HAProxy upstream")
			// Continue with other apps instead of failing completely
			continue
		}
	}

	return nil
}

// updateHAProxyUpstream reconciles the state of an HAProxy backend with the desired application hosts.
func updateHAProxyUpstream(client runtime_api.Runtime, app App) error {
	backend := app.Vhost

	// 1. Get the current state of servers from HAProxy
	currentServers, err := client.GetServersState(backend)
	if err != nil {
		logger.WithFields(logrus.Fields{
			"backend": backend,
			"error":   err,
		}).Warning("Cannot get servers for backend, it may not exist in the config. Skipping.")
		return nil // Not a fatal error, just skip this backend.
	}

	// 2. Build a map of the current servers for efficient lookup.
	currentServerMap := make(map[string]bool)
	for _, srv := range currentServers {
		currentServerMap[srv.Name] = true
	}

	// 3. Build a map of the desired servers from the application data.
	desiredServerMap := make(map[string]Host)
	for _, host := range app.Hosts {
		serverName := generateStableServerName(host)
		desiredServerMap[serverName] = host
	}

	// 4. Reconcile states: add/update new servers and disable/delete old ones.

	// Add or enable servers that are in the desired state.
	for serverName, desiredHost := range desiredServerMap {
		if _, exists := currentServerMap[serverName]; !exists {
			// Server does not exist, so we need to add it.
			// This requires a 'server-template' in your HAProxy config.
			logger.WithFields(logrus.Fields{
				"backend": backend,
				"server":  serverName,
			}).Info("Adding new server to HAProxy backend")
			if err := client.AddServer(backend, serverName, fmt.Sprintf("%s:%d", desiredHost.Host, desiredHost.Port)); err != nil {
				logger.WithFields(logrus.Fields{"backend": backend, "server": serverName, "error": err}).Error("Failed to add server")
			}
		} else {
			// Server exists, ensure it's enabled and the address is correct.
			logger.WithFields(logrus.Fields{"backend": backend, "server": serverName}).Trace("Enabling existing server")
			if err := client.SetServerAddr(backend, serverName, desiredHost.Host, int(desiredHost.Port)); err != nil {
				logger.WithFields(logrus.Fields{"backend": backend, "server": serverName, "error": err}).Error("Failed to set server address")
			}
			if err := client.SetServerState(backend, serverName, "ready"); err != nil {
				logger.WithFields(logrus.Fields{"backend": backend, "server": serverName, "error": err}).Error("Failed to set server state to ready")
			}
		}
	}

	// Disable servers that are no longer in the desired state.
	for _, srv := range currentServers {
		if _, exists := desiredServerMap[srv.Name]; !exists {
			logger.WithFields(logrus.Fields{
				"backend": backend,
				"server":  srv.Name,
			}).Info("Disabling stale server in HAProxy backend")
			if err := client.SetServerState(backend, srv.Name, "maint"); err != nil {
				logger.WithFields(logrus.Fields{"backend": backend, "server": srv.Name, "error": err}).Error("Failed to disable server")
			}
		}
	}

	return nil
}

// generateStableServerName creates a consistent, valid server name from a host.
func generateStableServerName(host Host) string {
	// Replace characters that are invalid in HAProxy server names.
	sanitizer := strings.NewReplacer(".", "_", ":", "_")
	return fmt.Sprintf("srv_%s_%d", sanitizer.Replace(host.Host), host.Port)
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
		isHTTPVHost := false
		for _, t := range app.Hosts {
			if (string(t.PortType) == "http") || (string(t.PortType) == "https") {
				isHTTPVHost = true
			}

		}

		if isHTTPVHost {
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
				// First add atleast one server to initialise upstream to support UpdateHTTPServers
				logger.WithFields(logrus.Fields{
					"Adding fresh upstream for": upstreamtocheck,
				}).Info("Adding first server for upstream")
				//Adding first server for server ID 0. ID 0 needs to be updated if state file is resurrected when a vhost gets resurrected. Create ID 0 otherwise.
				error := nginxClient.UpdateHTTPServer(upstreamtocheck, finalformattedServers[0])

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
					cancel()
				}

			}
			if err == nil {
				added, deleted, updated, error := nginxClient.UpdateHTTPServers(upstreamtocheck, finalformattedServers)

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
	if err != nil {
		return err
	}
	data = RenderingData{}
	createRenderingData(&data)
	err = t.Execute(io.Discard, &config)
	if err != nil {
		return err
	}
	return nil
}

func getTmpl(proxyTemplatePath string) (*template.Template, error) {
	logger.WithFields(logrus.Fields{
		"file": proxyTemplatePath,
	}).Info("Reading template")
	return template.New(filepath.Base(proxyTemplatePath)).
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
			"datetime":  time.Now}).
		ParseFiles(proxyTemplatePath)
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
	logger.Debug("Reloading haproxy with cmd: " + config.HaproxyReloadCmd)
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
