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

	haproxy_runtime_options "github.com/haproxytech/client-native/v6/runtime/options"
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
		//Nginx plus http_api is disabled
		upstreamUpdateAPIEnabled = false
	} else if (config.ProxyPlatform == "haproxy") && (len(config.HaproxySocketAddr) == 0 || config.HaproxySocketAddr == "") {
		//HAProxy Runtime API is disabled
		upstreamUpdateAPIEnabled = false
	}

	if !upstreamUpdateAPIEnabled {
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

		if config.ProxyPlatform == "nginx" {
			err = nginxPlus(&data)
		} else if config.ProxyPlatform == "haproxy" {
			//For HAProxy, config is generated but not loaded even when reload is disabled as there is not other way to persist state across reloads
			updateWithoutReloadConfig(&data)
			// err = haproxyRuntimeAPI(&data)
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
	config.LastUpdates.LastSync = time.Now()
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
	return nil
}

func updateAndReloadConfig(data *RenderingData, reloadDisabled bool) error {
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

func haproxyRuntimeAPI(data *RenderingData) error {
	//Current implementation only updates AppVhosts, does not suppport routing tag & LeaderVhost
	config.RLock()
	defer config.RUnlock()

	logger.WithFields(logrus.Fields{
		"haproxy": config.HaproxySocketAddr,
	}).Debug("haproxy socket address")

	ms := haproxy_runtime_options.MasterSocket(config.HaproxySocketAddr)
	client, err := haproxy_runtime.initWithMasterSocket.New(ms)

	if err != nil {
		return fmt.Errorf("error setting up runtime client: %s", err.Error())
	}
	env, err := client.ExecuteRaw("show env")
	if err != nil {
		logger.Println(err)
	}
	logger.Println(env)
	return nil
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

	logger.WithFields(logrus.Fields{"apps": data.Apps}).Debug("Updating upstreams for the whitelisted drove vhosts")
	for _, app := range data.Apps {
		var newFormattedServers []string
		for _, t := range app.Hosts {
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
	}
	return nil
}

func checkTmpl() error {
	config.RLock()
	defer config.RUnlock()
	t, err := getTmpl(TemplatePath)
	if err != nil {
		return err
	}
	data := RenderingData{}
	createRenderingData(&data)
	err = t.Execute(io.Discard, &data)
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
