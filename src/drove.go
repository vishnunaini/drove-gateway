package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

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

type DroveClient struct {
	namespace  string
	syncPoint  CurrSyncPoint
	httpClient *http.Client
}

type CurrSyncPoint struct {
	sync.RWMutex
	LastSyncTime int64
}

var droveClients map[string]*DroveClient

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

func fetchRecentEvents(httpClient *http.Client, syncPoint *CurrSyncPoint, namespace string) (*DroveEventSummary, error) {

	droveConfig, err := db.ReadDroveConfig(namespace)
	if err != nil {
		logger.WithFields(logrus.Fields{
			"namespace": namespace,
		}).Error("Error loading drove config")
		err := errors.New("error loading Drove Config")
		return nil, err
	}

	var endpoint string
	for _, es := range health.NamespaceEndpoints[namespace] {
		if es.Healthy {
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
	resp, err := httpClient.Do(req)
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

func refreshLeaderData(namespace string) bool {
	var endpoint string
	for _, es := range health.NamespaceEndpoints[namespace] {
		if es.Namespace == namespace && es.Healthy {
			endpoint = es.Endpoint
			break
		}
	}
	if endpoint == "" {
		logger.Error("all endpoints are down")
		go countAllEndpointsDownErrors.WithLabelValues(namespace).Inc()
		return false
	}
	currentLeader, err := db.ReadLeader(namespace)
	if err != nil {
		logger.Error("Error while reading current leader for namespace" + namespace)
		return false
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
			return true
		} else {
			logrus.Error("Leader struct generation failed")
		}
	}
	return false
}

func newDroveClient(name string) *DroveClient {
	return &DroveClient{namespace: name,
		syncPoint: CurrSyncPoint{},
		httpClient: &http.Client{
			Timeout:   time.Duration(config.apiTimeout) * time.Second,
			Transport: tr,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func pollingHandler(droveClient *DroveClient, appsConfigUpdateChannel chan<- bool, waitGroup *sync.WaitGroup) {
	defer waitGroup.Done()
	appsRefreshed := false
	namespace := droveClient.namespace
	logger.WithFields(logrus.Fields{
		"at":        time.Now(),
		"namespace": namespace,
	}).Debug("Syncing...")

	droveClient.syncPoint.Lock()
	defer droveClient.syncPoint.Unlock()

	leaderShifted := refreshLeaderData(namespace)
	eventSummary, err := fetchRecentEvents(droveClient.httpClient, &droveClient.syncPoint, namespace)
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
			appsConfigUpdateNeeded := false
			if _, ok := eventSummary.EventsCount["APP_STATE_CHANGE"]; ok {
				appsConfigUpdateNeeded = true
			}
			if _, ok := eventSummary.EventsCount["INSTANCE_STATE_CHANGE"]; ok {
				appsConfigUpdateNeeded = true
			}
			if appsConfigUpdateNeeded || leaderShifted {
				appsRefreshed = refreshApps(droveClient.httpClient, namespace, leaderShifted)
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
	appsConfigUpdateChannel <- appsRefreshed
}

func pollingEvents() {
	var waitGroup sync.WaitGroup
	appsConfigUpdateChannel := make(chan bool, len(droveClients))

	for _, droveClient := range droveClients {
		waitGroup.Add(1)
		go pollingHandler(droveClient, appsConfigUpdateChannel, &waitGroup)
	}

	waitGroup.Wait()
	close(appsConfigUpdateChannel)

	appsUpdated := false
	for result := range appsConfigUpdateChannel {
		if result {
			appsUpdated = true
			break
		}
	}
	logger.WithFields(logrus.Fields{
		"appsUpdated": appsUpdated,
	}).Info("Drove poll event result")

	if appsUpdated {
		appsConfigUpdateSignalQueue <- true
	}

}

func schedulePollDroveEvents() {
	go func() {
		ticker := time.NewTicker(time.Duration(config.EventRefreshIntervalSec) * time.Second)
		for {
			select {
			case <-ticker.C:
				logger.Info("Refreshing drove events as per schedule")
				pollingEvents()
			case <-eventRefreshSignalQueue:
				logger.Info("Refreshing drove events due to force referesh")
				pollingEvents()
			}
		}
	}()
}

func setupPollEvents() {
	droveClients = make(map[string]*DroveClient)
	for _, nsConfig := range config.DroveNamespaces {
		droveClients[nsConfig.Name] = newDroveClient(nsConfig.Name)
	}
	schedulePollDroveEvents()
}

func endpointHealthHandler(healthCheckClient *http.Client, namespace string) {
	for i, es := range health.NamespaceEndpoints[namespace] {
		req, err := http.NewRequest("GET", es.Endpoint+"/apis/v1/ping", nil)
		if err != nil {
			logger.WithFields(logrus.Fields{
				"error":    err.Error(),
				"endpoint": es.Endpoint,
			}).Error("an error occurred creating endpoint health request")
			health.NamespaceEndpoints[namespace][i].Healthy = false
			health.NamespaceEndpoints[namespace][i].Message = err.Error()
			continue
		}
		droveConfig, err := db.ReadDroveConfig(es.Namespace)
		if err != nil {
			logger.WithFields(logrus.Fields{
				"error":    err,
				"endpoint": es.Endpoint,
			}).Error("an error occurred reading drove config for health request")
			health.NamespaceEndpoints[namespace][i].Healthy = false
			health.NamespaceEndpoints[namespace][i].Message = err.Error()
			continue
		}
		if droveConfig.User != "" {
			req.SetBasicAuth(droveConfig.User, droveConfig.Pass)
		}
		if droveConfig.AccessToken != "" {
			req.Header.Add("Authorization", droveConfig.AccessToken)
		}
		resp, err := healthCheckClient.Do(req)
		if err != nil {
			logger.WithFields(logrus.Fields{
				"error":     err.Error(),
				"endpoint":  es.Endpoint,
				"namespace": namespace,
			}).Error("endpoint is down")
			go countEndpointDownErrors.WithLabelValues(namespace).Inc()
			health.NamespaceEndpoints[namespace][i].Healthy = false
			health.NamespaceEndpoints[namespace][i].Message = err.Error()
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			health.NamespaceEndpoints[namespace][i].Healthy = false
			health.NamespaceEndpoints[namespace][i].Message = resp.Status
			continue
		}
		health.NamespaceEndpoints[namespace][i].Healthy = true
		health.NamespaceEndpoints[namespace][i].Message = "OK"
		logger.WithFields(logrus.Fields{
			"host":      es.Endpoint,
			"namespace": namespace,
		}).Debug(" Endpoint is healthy")
	}
}

func endpointHealth(namespace string) {
	go func() {
		healthCheckClient := &http.Client{
			Timeout:   5 * time.Second,
			Transport: tr,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		ticker := time.NewTicker(2 * time.Second)
		for {
			select {
			case <-ticker.C:
				endpointHealthHandler(healthCheckClient, namespace)
			}
		}
	}()
}

func setupEndpointHealth() {
	for _, nsConfig := range config.DroveNamespaces {
		endpointHealth(nsConfig.Name)
	}
}

func fetchApps(httpClient *http.Client, droveConfig DroveConfig, jsonapps *DroveApps) error {
	var endpoint string
	for _, es := range health.NamespaceEndpoints[droveConfig.Name] {
		if es.Healthy {
			endpoint = es.Endpoint
			break
		}
	}
	if endpoint == "" {
		err := errors.New("all endpoints are down")
		return err
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
	resp, err := httpClient.Do(req)
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

func syncAppsAndVhosts(droveConfig DroveConfig, jsonapps *DroveApps, vhosts *Vhosts) bool {
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
						}).Debug("routing tag found")
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
				}).Debug("Host ignored due to realm mismatch")
			}
		}
	}

	currentApps, er := db.ReadApps(droveConfig.Name)
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

func refreshApps(httpClient *http.Client, namespace string, leaderShifted bool) bool {
	logger.Debug("Reloading config for namespace" + namespace)
	start := time.Now()
	droveConfig, er := db.ReadDroveConfig(namespace)
	if er != nil {
		logger.WithFields(logrus.Fields{
			"namespace": namespace,
		}).Error("Error loading drove config")
		return false
	}

	jsonapps := DroveApps{}
	vhosts := Vhosts{}
	vhosts.Vhosts = map[string]bool{}
	err := fetchApps(httpClient, droveConfig, &jsonapps)
	if err != nil || jsonapps.Status != "SUCCESS" {
		if err != nil {
			logger.WithFields(logrus.Fields{
				"error": err.Error(),
			}).Error("unable to sync from drove")
		}
		go statsCount("reload.failed", 1)
		go countDroveAppSyncErrors.WithLabelValues(namespace).Inc()
		return false
	}
	equal := syncAppsAndVhosts(droveConfig, &jsonapps, &vhosts)
	if equal && !leaderShifted {
		logger.Debug("no relevant App Data changes")
		return false
	}

	logger.WithFields(logrus.Fields{
		"namespace":     namespace,
		"leaderShifted": leaderShifted,
		"appsChanged":   !equal,
	}).Debug("Apps Data change required because of") //logging exact reason of potential reload

	elapsed := time.Since(start)
	go observeAppRefreshTimeMetric(namespace, elapsed)
	logger.WithFields(logrus.Fields{
		"took": elapsed,
	}).Debug("Apps update")
	return true
}
