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
			haproxyMgr, mgrErr := NewHaproxyManager(ctx, config.HaproxySocketAddr, config.HaproxyDisableLargeBackendCountOptimisation)
			if mgrErr != nil {
				err = mgrErr
			} else {
				err = haproxyMgr.ReconcileAllBackends(&data, config.HaproxyDisableLargeBackendCountOptimisation)
			}
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

func isHTTPHostGroup(hosts []Host) bool {
	for _, host := range hosts {
		if host.PortType != "http" && host.PortType != "https" {
			return false
		}
	}
	return true
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
					ctxDns, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					ipRecord, error := resolveHostnameToIP(ctxDns, string(t.Host))
					cancel()
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
