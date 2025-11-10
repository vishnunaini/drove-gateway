package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	nplus "github.com/nginxinc/nginx-plus-go-client/client"
	"github.com/sirupsen/logrus"
)

type NginxAPIManager struct {
	client             *nplus.NginxClient
	apiAddr            string
	apiTimeout         time.Duration
	upstreamParameters struct {
		MaxFails    *int
		FailTimeout string
		SlowStart   string
	}
}

func NewNginxAPIManager(nginxPlusApiAddr string, apiTimeout time.Duration, MaxFails *int, FailTimeout string, SlowStart string) (*NginxAPIManager, error) {
	endpoint := "http://" + nginxPlusApiAddr + "/api"
	tr := &http.Transport{
		MaxIdleConns:       30,
		DisableCompression: true,
	}
	client := &http.Client{Transport: tr}
	nginxClient, err := nplus.NewNginxClient(endpoint, nplus.WithHTTPClient(client), nplus.WithAPIVersion(8))
	if err != nil {
		logger.WithFields(logrus.Fields{
			"error": err,
		}).Error("unable to make call to nginx plus")
		updateHealthForUpstreamUpdateAPI(false, err.Error())
		return nil, err
	}
	return &NginxAPIManager{client: nginxClient, apiAddr: nginxPlusApiAddr, apiTimeout: apiTimeout, upstreamParameters: struct {
		MaxFails    *int
		FailTimeout string
		SlowStart   string
	}{
		MaxFails:    MaxFails,
		FailTimeout: FailTimeout,
		SlowStart:   SlowStart,
	}}, nil
}

// ReconcileAllVhosts updates all HTTP vhosts using the NGINX Plus API.
func (m *NginxAPIManager) ReconcileAllVhosts(data *RenderingData) error {
	start := time.Now()
	var resultLabel string
	defer func() {
		duration := time.Since(start)
		nginxPlusReconcileAllBackendsDuration.WithLabelValues(resultLabel).Observe(duration.Seconds())
	}()

	logger.WithFields(logrus.Fields{
		"nginx": m.apiAddr,
	}).Debug("NGINX Plus API endpoint")

	reconciledApps := make(map[string]bool)
	reconciliationFailedApps := make(map[string]bool)

	for _, app := range data.Apps {
		if !isHTTPHostGroup(app.Hosts) || app.Vhost == "" || len(app.Hosts) == 0 {
			logger.WithFields(logrus.Fields{"vhost": app.Vhost}).Debug("Skipping non-HTTP/S vhost update")
			continue
		}

		var newFormattedServers []string
		for _, t := range app.Hosts {
			if t.PortType == "http" || t.PortType == "https" {
				ipRecord, err := resolveWithIPFallback(t.Host)
				if err != nil {
					logger.WithFields(logrus.Fields{
						"error":    err,
						"hostname": t.Host,
					}).Error("dns lookup failed, skipping the hostname")
					reconciliationFailedApps[app.Vhost] = true
					updateHealthForUpstreamUpdateAPI(false, err.Error())
					continue
				}
				hostAndPortMapping := fmt.Sprintf("%s:%d", ipRecord, t.Port)
				newFormattedServers = append(newFormattedServers, hostAndPortMapping)
			}
		}

		logger.WithFields(logrus.Fields{
			"vhost":     app.Vhost,
			"upstreams": newFormattedServers,
		}).Debug("nginx upstreams")

		upstreamtocheck := app.Vhost
		var finalformattedServers []nplus.UpstreamServer
		for _, server := range newFormattedServers {
			finalformattedServers = append(finalformattedServers, nplus.UpstreamServer{
				Server:      server,
				MaxFails:    m.upstreamParameters.MaxFails,
				FailTimeout: m.upstreamParameters.FailTimeout,
				SlowStart:   m.upstreamParameters.SlowStart,
			})
		}

		err := m.client.CheckIfUpstreamExists(upstreamtocheck)
		if err != nil {
			nginxAPICallsFailed.WithLabelValues("check_if_upstream_exists").Inc()
			logger.WithFields(logrus.Fields{
				"Adding fresh upstream for": upstreamtocheck,
			}).Info("Adding first server for upstream")
			if len(finalformattedServers) == 0 {
				logger.WithFields(logrus.Fields{
					"vhost": upstreamtocheck,
				}).Warn("No servers to add for new upstream")
				continue
			}
			addErr := m.client.UpdateHTTPServer(upstreamtocheck, finalformattedServers[0])
			if addErr != nil {
				nginxAPICallsFailed.WithLabelValues("update_http_server").Inc()
			} else {
				nginxAPICallsSuccessful.WithLabelValues("update_http_server").Inc()
			}
			// Wait for upstream to exist
			if addErr != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
				defer cancel()
				for {
					select {
					case <-ctx.Done():
						err = fmt.Errorf("context timeout waiting for upstream '%s' to exist", upstreamtocheck)
						logger.WithError(err).Error("Failed to confirm upstream creation")
						updateHealthForUpstreamUpdateAPI(false, err.Error())
						break
					default:
						time.Sleep(5 * time.Millisecond)
						if m.client.CheckIfUpstreamExists(upstreamtocheck) == nil {
							nginxAPICallsSuccessful.WithLabelValues("check_if_upstream_exists").Inc()
							break
						}
					}
				}
			}
		}

		if err == nil {
			nginxAPICallsSuccessful.WithLabelValues("check_if_upstream_exists").Inc()
			added, deleted, updated, updateErr := m.client.UpdateHTTPServers(upstreamtocheck, finalformattedServers)
			if updateErr != nil {
				nginxAPICallsFailed.WithLabelValues("update_http_servers").Inc()
			} else {
				nginxAPICallsSuccessful.WithLabelValues("update_http_servers").Inc()
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
			if updateErr != nil {
				logger.WithFields(logrus.Fields{
					"vhost": upstreamtocheck,
					"error": updateErr,
				}).Error("unable to update nginx upstreams")
				return updateErr
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
