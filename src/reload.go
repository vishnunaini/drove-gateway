package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/sirupsen/logrus"
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
	if len(config.Nginxplusapiaddr) == 0 || config.Nginxplusapiaddr == "" {
		//Nginx plus is disabled
		err = updateAndReloadConfig(&data)
		if err != nil {
			logger.WithFields(logrus.Fields{
				"error": err.Error(),
			}).Error("unable to reload nginx config")
			go statsCount("reload.failed", 1)
			go countFailedReloads.Inc()
			return err
		}
	} else {
		//Nginx plus is enabled
		if config.NginxReloadDisabled {
			logger.Warn("Template reload has been disabled")
		} else {
			logger.Info("Need to reload config")
			err = updateAndReloadConfig(&data)
			if err != nil {
				logger.WithFields(logrus.Fields{
					"error": err.Error(),
				}).Error("unable to update and reload nginx config. NPlus api calls will be skipped.")
				return err
			} else {
				logger.Debug("No changes detected in vhosts. No config update is necessary. Upstream updates will happen via nplus apis")
			}
		}
		err = nginxPlus(&data)
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
	}).Info("config reloaded successfully")
	return nil

}

func updateAndReloadConfig(data *RenderingData) error {
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
	err = reloadNginx()
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
		}).Info("config updated successfully")
		go statsCount("reload.success", 1)
		go statsTiming("reload.time", elapsed)
		go countSuccessfulReloads.Inc()
		go observeReloadTimeMetric(elapsed)
		config.LastUpdates.LastNginxReload = time.Now()
		db.UpdateLastKnownVhosts(vhosts)
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
	}).Info("Rendering data generated")
	return
}

func writeConf(data *RenderingData) error {
	config.RLock()
	defer config.RUnlock()

	template, err := getTmpl()
	if err != nil {
		return err
	}

	parent := filepath.Dir(config.NginxConfig)
	tmpFile, err := ioutil.TempFile(parent, ".nginx.conf.tmp-")
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
	err = os.Rename(tmpFile.Name(), config.NginxConfig)
	if err != nil {
		return err
	}
	lastConfig = config.NginxConfig
	return nil
}

func nginxPlus(data *RenderingData) error {
	//Current implementation only updates AppVhosts, does not suppport routing tag & LeaderVhost
	config.RLock()
	defer config.RUnlock()
	logger.WithFields(logrus.Fields{"apps": data.Apps}).Info("Updating upstreams for the whitelisted drove tags")
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
		}).Info("app.vhost")

		logger.WithFields(logrus.Fields{
			"upstreams": newFormattedServers,
		}).Info("nginx upstreams")

		logger.WithFields(logrus.Fields{
			"nginx": config.Nginxplusapiaddr,
		}).Info("endpoint")

		endpoint := "http://" + config.Nginxplusapiaddr + "/api"

		tr := &http.Transport{
			MaxIdleConns:       30,
			DisableCompression: true,
		}

		client := &http.Client{Transport: tr}
		c := NginxClient{endpoint, client}
		nginxClient, error := NewNginxClient(c.httpClient, c.apiEndpoint)
		if error != nil {
			logger.WithFields(logrus.Fields{
				"error": error,
			}).Error("unable to make call to nginx plus")
			return error
		}
		upstreamtocheck := app.Vhost
		var finalformattedServers []UpstreamServer

		for _, server := range newFormattedServers {
			formattedServer := UpstreamServer{Server: server, MaxFails: config.MaxFailsUpstream, FailTimeout: config.FailTimeoutUpstream, SlowStart: config.SlowStartUpstream}
			finalformattedServers = append(finalformattedServers, formattedServer)
		}

		added, deleted, updated, error := nginxClient.UpdateHTTPServers(upstreamtocheck, finalformattedServers)

		if added != nil {
			logger.WithFields(logrus.Fields{
				"nginx upstreams added": added,
			}).Info("nginx upstreams added")
		}
		if deleted != nil {
			logger.WithFields(logrus.Fields{
				"nginx upstreams deleted": deleted,
			}).Info("nginx upstreams deleted")
		}
		if updated != nil {
			logger.WithFields(logrus.Fields{
				"nginx upsteams updated": updated,
			}).Info("nginx upstreams updated")
		}
		if error != nil {
			logger.WithFields(logrus.Fields{
				"error": error,
			}).Error("unable to update nginx upstreams")
			return error
		}
	}
	return nil
}

func checkTmpl() error {
	config.RLock()
	defer config.RUnlock()
	t, err := getTmpl()
	if err != nil {
		return err
	}
	err = t.Execute(ioutil.Discard, &config)
	if err != nil {
		return err
	}
	return nil
}

func getTmpl() (*template.Template, error) {
	logger.WithFields(logrus.Fields{
		"file": config.NginxTemplate,
	}).Info("Reading template")
	return template.New(filepath.Base(config.NginxTemplate)).
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
		ParseFiles(config.NginxTemplate)
}

func checkConf(path string) error {
	// Always return OK if disabled in config.
	if config.NginxIgnoreCheck {
		return nil
	}
	// This is to allow arguments as well. Example "docker exec nginx..."
	args := strings.Fields(config.NginxCmd)
	head := args[0]
	args = args[1:]
	args = append(args, "-c")
	args = append(args, path)
	args = append(args, "-t")
	cmd := exec.Command(head, args...)
	//cmd := exec.Command(parts..., "-c", path, "-t")
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

func reloadWorker() {
	go func() {
		// a ticker channel to limit reloads to drove, 1s is enough for now.
		ticker := time.NewTicker(1 * time.Second)
		for {
			select {
			case <-ticker.C:
				<-reloadSignalQueue
				reload()
			}
		}
	}()
}
