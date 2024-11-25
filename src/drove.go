package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/sirupsen/logrus"
)

type NamespaceRenderingData struct {
	LeaderVHost string `json:"-" toml:"leader_vhost"`
	Leader      LeaderController
	Apps        map[string]App
	RoutingTag  string `json:"-" toml:"routing_tag"`
	KnownVHosts Vhosts
}

type TemplateRenderingData struct {
	Xproxy              string
	LeftDelimiter       string                            `json:"-" toml:"left_delimiter"`
	RightDelimiter      string                            `json:"-" toml:"right_delimiter"`
	MaxFailsUpstream    *int                              `json:"max_fails,omitempty"`
	FailTimeoutUpstream string                            `json:"fail_timeout,omitempty"`
	SlowStartUpstream   string                            `json:"slow_start,omitempty"`
	Namespaces          map[string]NamespaceRenderingData `json:"namespaces"`
}

// DroveApps struct for our apps nested with tasks.
type DroveApps struct {
	Status string `json:"status"`
	Apps   []struct {
		ID      string            `json:"appId"`
		Vhost   string            `json:"vhost"`
		Tags    map[string]string `json:"tags"`
		AppName string            `json:"appName"`
		Hosts   []struct {
			Host     string `json:"host"`
			Port     int32  `json:"port"`
			PortType string `json:"portType"`
		} `json:"hosts"`
	} `json:"data"`
	Message string `json:"message"`
}

type DroveEventSummary struct {
	EventsCount  map[string]interface{} `json:"eventsCount"`
	LastSyncTime int64                  `json:"lastSyncTime"`
}

type DroveEventsApiResponse struct {
	Status       string            `json:"status"`
	EventSummary DroveEventSummary `json:"data"`
	Message      string            `json:"message"`
}

type CurrSyncPoint struct {
	sync.RWMutex
	LastSyncTime int64
}

func leaderController(endpoint string) *LeaderController {
	if endpoint == "" {
		return nil
	}
	var controllerHost = new(LeaderController)
	controllerHost.Endpoint = endpoint
	var parsedUrl, err = url.Parse(endpoint)
	if err != nil {
		logger.WithFields(logrus.Fields{
			"error": err.Error(),
			"url":   endpoint,
		}).Error("could not parse endpoint url")
		return nil
	}
	var host, port, splitErr = net.SplitHostPort(parsedUrl.Host)
	if splitErr != nil {
		logger.WithFields(logrus.Fields{
			"error": splitErr.Error(),
			"url":   endpoint,
		}).Error("could not parse endpoint url")
		return nil
	}
	controllerHost.Host = host
	var iPort, _ = strconv.Atoi(port)
	controllerHost.Port = int32(iPort)
	return controllerHost
}

func fetchRecentEvents(client *http.Client, syncPoint *CurrSyncPoint, namespace string) (*DroveEventSummary, error) {

	droveConfig, err := db.ReadDroveConfig(namespace)
	if err != nil {
		logger.WithFields(logrus.Fields{
			"namespace": namespace,
		}).Error("Error loading drove config")
		err := errors.New("Error loading Drove Config")
		return nil, err
	}

	var endpoint string
	for _, es := range health.Endpoints {
		if es.Healthy && es.Namespace == namespace {
			endpoint = es.Endpoint
			break
		}
	}
	if endpoint == "" {
		err := errors.New("all endpoints are down")
		return nil, err
	}

	// fetch all apps and tasks with a single request.
	req, err := http.NewRequest("GET", endpoint+"/apis/v1/cluster/events/summary?lastSyncTime="+fmt.Sprint(syncPoint.LastSyncTime), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if droveConfig.User != "" {
		req.SetBasicAuth(droveConfig.User, droveConfig.Pass)
	}
	if droveConfig.AccessToken != "" {
		req.Header.Add("Authorization", droveConfig.AccessToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	decoder := json.NewDecoder(resp.Body)
	var newEventsApiResponse = DroveEventsApiResponse{}

	err = decoder.Decode(&newEventsApiResponse)
	if err != nil {
		return nil, err
	}
	logger.WithFields(logrus.Fields{
		"data":      newEventsApiResponse,
		"namespace": namespace,
	}).Debug("events response")
	if newEventsApiResponse.Status != "SUCCESS" {
		return nil, errors.New("Events api call failed. Message: " + newEventsApiResponse.Message)
	}

	syncPoint.LastSyncTime = newEventsApiResponse.EventSummary.LastSyncTime
	return &(newEventsApiResponse.EventSummary), nil
}

func refreshLeaderData(namespace string) {
	var endpoint string
	for _, es := range health.Endpoints {
		if es.Namespace == namespace && es.Healthy {
			endpoint = es.Endpoint
			break
		}
	}
	if endpoint == "" {
		logger.Error("all endpoints are down")
		go countAllEndpointsDownErrors.Inc()
		return
	}
	currentLeader, err := db.ReadLeader(namespace)
	if err != nil {
		logger.Error("Error while reading current leader for namespace" + namespace)
		return
	}
	if endpoint != currentLeader.Endpoint {
		logger.WithFields(logrus.Fields{
			"new": endpoint,
			"old": currentLeader.Endpoint,
		}).Info("Looks like master shifted. Will resync app")
		var newLeader = leaderController(endpoint)
		if newLeader != nil {
			err = db.UpdateLeader(namespace, *newLeader)
			if err != nil {
				logger.WithFields(logrus.Fields{
					"leader":    currentLeader,
					"newLeader": newLeader,
					"namespace": namespace,
				}).Error("Error seting new leader")
			}
			logger.WithFields(logrus.Fields{
				"leader":    currentLeader,
				"newLeader": newLeader,
			}).Info("New leader being set")
			reloadAllApps(namespace, true)
		} else {
			logrus.Warn("Leade struct generation failed")
		}
	}
}

func pollEvents(namespace string) {
	go func() {
		client := &http.Client{
			Timeout:   0 * time.Second,
			Transport: tr,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		syncData := CurrSyncPoint{}
		refreshInterval := 2
		if config.EventRefreshIntervalSec > 0 {
			refreshInterval = config.EventRefreshIntervalSec
		}
		ticker := time.NewTicker(time.Duration(refreshInterval) * time.Second)
		for range ticker.C {
			func() {
				logger.WithFields(logrus.Fields{
					"at": time.Now(),
				}).Debug("Syncing...")
				syncData.Lock()
				defer syncData.Unlock()
				refreshLeaderData(namespace)
				eventSummary, err := fetchRecentEvents(client, &syncData, namespace)
				if err != nil {
					logger.WithFields(logrus.Fields{
						"error":     err.Error(),
						"namespace": namespace,
					}).Error("unable to sync events from drove")
				} else {
					if len(eventSummary.EventsCount) > 0 {
						logger.WithFields(logrus.Fields{
							"events":    eventSummary.EventsCount,
							"namespace": namespace,
							"localTime": time.Now(),
						}).Info("Events received")
						reloadNeeded := false
						if _, ok := eventSummary.EventsCount["APP_STATE_CHANGE"]; ok {
							reloadNeeded = true
						}
						if _, ok := eventSummary.EventsCount["INSTANCE_STATE_CHANGE"]; ok {
							reloadNeeded = true
						}
						if reloadNeeded {
							select {
							case eventqueue <- true: // add reload to our queue channel, unless it is full of course.
							default:
								logger.Warn("queue is full")
							}
						} else {
							logger.Debug("Irrelevant events ignored")
						}
					} else {
						logger.WithFields(logrus.Fields{
							"events":    eventSummary.EventsCount,
							"namespace": namespace,
						}).Debug("New Events received")
					}
				}
			}()
		}
	}()
}

func eventWorker() {
	go func() {
		// a ticker channel to limit reloads to drove, 1s is enough for now.
		ticker := time.NewTicker(1 * time.Second)
		for {
			select {
			case <-ticker.C:
				<-eventqueue
				reload()
			}
		}
	}()
}

func endpointHealth() {
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		for {
			select {
			case <-ticker.C:
				//logger.WithFields(logrus.Fields{
				//            "health": health,
				//}).Info("Reloading endpoint health")
				for i, es := range health.Endpoints {
					client := &http.Client{
						Timeout:   5 * time.Second,
						Transport: tr,
						CheckRedirect: func(req *http.Request, via []*http.Request) error {
							return http.ErrUseLastResponse
						},
					}
					req, err := http.NewRequest("GET", es.Endpoint+"/apis/v1/ping", nil)
					if err != nil {
						logger.WithFields(logrus.Fields{
							"error":    err.Error(),
							"endpoint": es.Endpoint,
						}).Error("an error occurred creating endpoint health request")
						health.Endpoints[i].Healthy = false
						health.Endpoints[i].Message = err.Error()
						continue
					}
					droveConfig, err := db.ReadDroveConfig(es.Namespace)
					if err != nil {
						logger.WithFields(logrus.Fields{
							"error":    err,
							"endpoint": es.Endpoint,
						}).Error("an error occurred reading drove config for health request")
						health.Endpoints[i].Healthy = false
						health.Endpoints[i].Message = err.Error()
						continue
					}
					if droveConfig.User != "" {
						req.SetBasicAuth(droveConfig.User, droveConfig.Pass)
					}
					if droveConfig.AccessToken != "" {
						req.Header.Add("Authorization", droveConfig.AccessToken)
					}
					resp, err := client.Do(req)
					if err != nil {
						logger.WithFields(logrus.Fields{
							"error":    err.Error(),
							"endpoint": es.Endpoint,
						}).Error("endpoint is down")
						go countEndpointDownErrors.Inc()
						health.Endpoints[i].Healthy = false
						health.Endpoints[i].Message = err.Error()
						continue
					}
					resp.Body.Close()
					if resp.StatusCode != 200 {
						health.Endpoints[i].Healthy = false
						health.Endpoints[i].Message = resp.Status
						continue
					}
					health.Endpoints[i].Healthy = true
					health.Endpoints[i].Message = "OK"
					logger.WithFields(logrus.Fields{"host": es.Endpoint}).Debug(" Endpoint is healthy")
				}
			}
		}
	}()
}

func fetchApps(droveConfig DroveConfig, jsonapps *DroveApps) error {
	var endpoint string
	var timeout int = 5
	for _, es := range health.Endpoints {
		if es.Healthy && droveConfig.Name == es.Namespace {
			endpoint = es.Endpoint
			break
		}
	}
	if endpoint == "" {
		err := errors.New("all endpoints are down")
		return err
	}
	if config.apiTimeout != 0 {
		timeout = config.apiTimeout
	}
	client := &http.Client{
		Timeout:   time.Duration(timeout) * time.Second,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	// fetch all apps and tasks with a single request.
	req, err := http.NewRequest("GET", endpoint+"/apis/v1/endpoints", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if droveConfig.User != "" {
		req.SetBasicAuth(droveConfig.User, droveConfig.Pass)
	}
	if droveConfig.AccessToken != "" {
		req.Header.Add("Authorization", droveConfig.AccessToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	decoder := json.NewDecoder(resp.Body)
	err = decoder.Decode(&jsonapps)
	if err != nil {
		return err
	}
	return nil
}

func matchingVhost(vHost string, realms []string) bool {
	if len(realms) == 0 {
		return true
	}
	for _, realm := range realms {
		if vHost == strings.TrimSpace(realm) {
			return true
		}
	}
	return false
}

func syncApps(droveConfig DroveConfig, jsonapps *DroveApps, vhosts *Vhosts) bool {
	config.Lock()
	defer config.Unlock()
	apps := make(map[string]App)
	var realms = []string{}
	if len(droveConfig.Realm) > 0 {
		realms = strings.Split(droveConfig.Realm, ",")
	}
	for _, app := range jsonapps.Apps {
		var newapp = App{}
		for _, task := range app.Hosts {
			var newtask = Host{}
			newtask.Host = task.Host
			newtask.Port = task.Port
			newtask.PortType = strings.ToLower(task.PortType)
			newapp.Hosts = append(newapp.Hosts, newtask)
		}
		// Lets ignore apps if no instances are available
		if len(newapp.Hosts) > 0 {
			var toAppend = matchingVhost(app.Vhost, realms) || (len(droveConfig.RealmSuffix) > 0 && strings.HasSuffix(app.Vhost, droveConfig.RealmSuffix))
			if toAppend {
				vhosts.Vhosts[app.Vhost] = true
				newapp.ID = app.Vhost
				newapp.Vhost = app.Vhost

				var groupName = app.Vhost
				if len(droveConfig.RoutingTag) > 0 {
					if tagValue, ok := app.Tags[droveConfig.RoutingTag]; ok {
						logger.WithFields(logrus.Fields{
							"tag":   droveConfig.RoutingTag,
							"vhost": newapp.Vhost,
							"value": tagValue,
						}).Info("routing tag found")
						groupName = tagValue
					} else {

						logger.WithFields(logrus.Fields{
							"tag":   droveConfig.RoutingTag,
							"vhost": newapp.Vhost,
						}).Debug("no routing tag found")
					}
				} else {
					logrus.Debug("No routing tag found")
				}

				var hostGroup = HostGroup{}
				hostGroup.Tags = app.Tags
				hostGroup.Hosts = newapp.Hosts

				newapp.Tags = app.Tags
				if existingApp, ok := apps[app.Vhost]; ok {
					newapp.Groups = existingApp.Groups
					if existingGroup, ok := newapp.Groups[groupName]; ok {
						existingGroup.Hosts = append(newapp.Hosts, existingGroup.Hosts...)
					} else {
						newapp.Groups[groupName] = hostGroup
					}
					newapp.Hosts = append(newapp.Hosts, existingApp.Hosts...)
					if newapp.Tags == nil {
						newapp.Tags = make(map[string]string)
					}
					for tn, tv := range existingApp.Tags {
						newapp.Tags[tn] = tv
					}
				} else {
					newapp.Groups = make(map[string]HostGroup)
					newapp.Groups[groupName] = hostGroup
				}
				apps[app.Vhost] = newapp
			} else {
				logger.WithFields(logrus.Fields{
					"realm": droveConfig.Realm,
					"vhost": app.Vhost,
				}).Warn("Host ignored due to realm mismath")
			}
		}
	}

	currentApps, er := db.ReadApp(droveConfig.Name)
	if er != nil {
		logger.Error("Error while reading apps for namespace" + droveConfig.Name)
		//Continue to update with latest data
	}

	// Not all events bring changes, so lets see if anything is new.
	eq := reflect.DeepEqual(apps, currentApps)
	if eq {
		return true
	}

	err := db.UpdateApps(droveConfig.Name, apps)
	if err != nil {
		logger.Error("Error while updating apps for namespace" + droveConfig.Name)
		return true
	}

	er = db.UpdateKnownVhosts(droveConfig.Name, *vhosts)
	if er != nil {
		logger.Error("Error while updating KnowVhosts for namespace" + droveConfig.Name)
		return true
	}
	return false
}

func createTemplateData(templateData *TemplateRenderingData) {
	namespaceData := db.ReadAllNamespace()
	staticData := db.ReadStaticData()

	templateData.Xproxy = staticData.Xproxy
	templateData.LeftDelimiter = staticData.LeftDelimiter
	templateData.RightDelimiter = staticData.RightDelimiter
	templateData.FailTimeoutUpstream = staticData.FailTimeoutUpstream
	templateData.MaxFailsUpstream = staticData.MaxFailsUpstream
	templateData.SlowStartUpstream = staticData.SlowStartUpstream

	templateData.Namespaces = make(map[string]NamespaceRenderingData)

	for name, data := range namespaceData {
		templateData.Namespaces[name] = NamespaceRenderingData{
			LeaderVHost: data.Drove.LeaderVHost,
			Leader:      data.Leader,
			Apps:        data.Apps,
			KnownVHosts: data.KnownVHosts,
			RoutingTag:  data.Drove.RoutingTag,
		}
	}
	return
}

func writeConf() error {
	config.RLock()
	defer config.RUnlock()
	allApps := db.ReadAllApps()
	allLeaders := db.ReadAllLeaders()

	template, err := getTmpl()
	if err != nil {
		return err
	}
	logger.WithFields(logrus.Fields{
		"apps: ": allApps,
		"leader": allLeaders,
	}).Info("Config: ")

	parent := filepath.Dir(config.NginxConfig)
	tmpFile, err := ioutil.TempFile(parent, ".nginx.conf.tmp-")
	if err != nil {
		return err
	}
	defer tmpFile.Close()
	lastConfig = tmpFile.Name()
	templateData := TemplateRenderingData{}
	createTemplateData(&templateData)
	logger.WithFields(logrus.Fields{
		"templateData": templateData,
	}).Info("Template Data generated")
	err = template.Execute(tmpFile, &templateData)
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

func nginxPlus() error {
	config.RLock()
	defer config.RUnlock()
	allApps := db.ReadAllApps()
	logger.WithFields(logrus.Fields{}).Info("Updating upstreams for the whitelisted drove tags")
	for _, app := range allApps {
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

func reload() error {
	return reloadAllApps(config.DroveNamespaces[0].Name, false) //TODO: for for all namespace
}

func updateAndReloadConfig() error {
	start := time.Now()
	config.LastUpdates.LastSync = time.Now()
	vhosts := db.ReadAllKnownVhosts()
	err := writeConf()
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
		}).Info("config updated")
		go statsCount("reload.success", 1)
		go statsTiming("reload.time", elapsed)
		go countSuccessfulReloads.Inc()
		go observeReloadTimeMetric(elapsed)
		config.LastUpdates.LastNginxReload = time.Now()
		db.UpdateLastKnownVhosts(vhosts)
	}
	return nil
}

func reloadAllApps(namespace string, leaderShifted bool) error {
	logger.Debug("Reloading config for namespace" + namespace)

	droveConfig, er := db.ReadDroveConfig(namespace)
	if er != nil {
		logger.WithFields(logrus.Fields{
			"namespace": namespace,
		}).Error("Error loading drove config")
		er := errors.New("Error loading Drove Config")
		return er
	}

	start := time.Now()
	jsonapps := DroveApps{}
	vhosts := Vhosts{}
	vhosts.Vhosts = map[string]bool{}
	err := fetchApps(droveConfig, &jsonapps)
	if err != nil || jsonapps.Status != "SUCCESS" {
		if err != nil {
			logger.WithFields(logrus.Fields{
				"error": err.Error(),
			}).Error("unable to sync from drove")
		}
		go statsCount("reload.failed", 1)
		go countFailedReloads.Inc()
		return err
	}
	equal := syncApps(droveConfig, &jsonapps, &vhosts)
	if equal && !leaderShifted {
		logger.Debug("no config changes")
		return nil
	}
	config.LastUpdates.LastSync = time.Now()
	if len(config.Nginxplusapiaddr) == 0 || config.Nginxplusapiaddr == "" {
		//Nginx plus is disabled
		err = updateAndReloadConfig()
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
			err = updateAndReloadConfig()
			if err != nil {
				logger.WithFields(logrus.Fields{
					"error": err.Error(),
				}).Error("unable to update and reload nginx config. NPlus api calls will be skipped.")
				return err
			} else {
				logger.Debug("No changes detected in vhosts. No config update is necessary. Upstream updates will happen via nplus apis")
			}
		}
		err = nginxPlus()
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
	}).Info("config updated")
	return nil
}
