package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

type ProxyManager interface {
	GenerateStableBackendName(app App, groupName string) string
	GenerateStableServerName(host Host) string
	CheckConfig() error
	GetTempFilePattern() string
	Reconcile(data *RenderingData) error
	UpdateAPIUpdatesHealthStatus(status bool, message string)
	Reload() error
}

type NginxProxyManager struct {
	config     *Config
	apiManager *NginxAPIManager
}

func (pmgr *NginxProxyManager) CheckConfig() error {
	// This is to allow arguments as well. Example "docker exec nginx..."
	args := strings.Fields(ProgramCmd)
	head := args[0]
	args = args[1:]
	args = append(args, ProgramCmdConfFileArg, ConfigPath, ProgramCmdConfTestArg)
	return runCommand(head, args...)
}

func (pmgr *NginxProxyManager) GetTempFilePattern() string {
	return ".nginx.conf.tmp-"
}

func (pmgr *NginxProxyManager) Reconcile(data *RenderingData) error {
	return pmgr.apiManager.ReconcileAllVhosts(data)
}

func (pmgr *NginxProxyManager) UpdateAPIUpdatesHealthStatus(status bool, message string) {
	updateHealthSection("UpstreamUpdatesAPI", status, message)
}

func (pmgr *NginxProxyManager) Reload() error {
	if pmgr.config.NginxIgnoreCheck {
		return nil
	}
	if err := pmgr.CheckConfig(); err != nil {
		return err
	}
	// This is to allow arguments as well. Example "docker exec nginx..."
	args := strings.Fields(pmgr.config.NginxCmd)
	head := args[0]
	args = args[1:]
	args = append(args, "-s", "reload")
	logger.WithFields(logrus.Fields{"cmd": head, "args": args}).Info("Reloading nginx")
	return runCommand(head, args...)
}

// GenerateStableBackendName returns a stable backend name for the given app and group.
func (pmgr *NginxProxyManager) GenerateStableBackendName(app App, groupName string) string {
	// For NGINX, backend name is just the vhost
	return app.Vhost
}

// GenerateStableServerName returns a stable server name for the given host (NGINX).
func (pmgr *NginxProxyManager) GenerateStableServerName(host Host) string {
	// For NGINX, server name is just host:port (or whatever is needed, adjust as per your conventions)
	return fmt.Sprintf("%s:%d", host.Host, host.Port)
}

type HAProxyManager struct {
	config     *Config
	apiManager *HaproxyManager
}

func (pmgr *HAProxyManager) CheckConfig() error {
	if pmgr.config.HaproxyIgnoreCheck {
		return nil
	}
	// This is to allow arguments as well. Example "docker exec nginx..."
	args := strings.Fields(ProgramCmd)
	head := args[0]
	args = args[1:]
	args = append(args, ProgramCmdConfFileArg, ConfigPath, ProgramCmdConfTestArg)
	return runCommand(head, args...)
}

func (pmgr *HAProxyManager) GetTempFilePattern() string {
	return ".haproxy.conf.tmp-"
}

func (pmgr *HAProxyManager) Reconcile(data *RenderingData) error {
	//For HAProxy, config is generated but not loaded even when reload is disabled as there is not other way to persist state across reloads
	updateProxyConfig(data)

	return pmgr.apiManager.ReconcileAllBackends(data, config.HaproxyDisableLargeBackendCountOptimisation)
}

func (pmgr *HAProxyManager) UpdateAPIUpdatesHealthStatus(status bool, message string) {
	updateHealthSection("UpstreamUpdatesAPI", status, message)
}

func (pmgr *HAProxyManager) Reload() error {
	if pmgr.config.HaproxyIgnoreCheck {
		return nil
	}
	if err := pmgr.CheckConfig(); err != nil {
		return err
	}
	// This is to allow arguments as well. Example "docker exec nginx..." or SIGUSR2 to master worker
	args := strings.Fields(pmgr.config.HaproxyReloadCmd)
	head := args[0]
	args = args[1:]
	logger.WithFields(logrus.Fields{"cmd": head, "args": args}).Info("Reloading haproxy")
	return runCommand(head, args...)
}

// GenerateStableBackendName returns a stable backend name for the given app and group (HAProxy).
func (pmgr *HAProxyManager) GenerateStableBackendName(app App, groupName string) string {
	vhost := app.Vhost
	routingTagKey := app.RoutingTagKey
	routingTagValue := groupName
	// Only add suffix if the feature is enabled and a routing tag key is configured.
	if pmgr.config.HaproxyBackendIncludeRoutingTagSuffix && routingTagKey != "" {
		return fmt.Sprintf("%s%s%s", vhost, pmgr.config.HaproxyBackendNameSeparator, routingTagValue)
	}
	// Fallback to just the vhost if no routing tag is found or configured.
	return vhost
}

// GenerateStableServerName returns a stable server name for the given host (HAProxy).
func (pmgr *HAProxyManager) GenerateStableServerName(host Host) string {
	// Replace characters that are invalid in HAProxy server names.
	sanitizer := strings.NewReplacer(":", pmgr.config.HaproxyServerNameHostPortSeparator)
	return fmt.Sprintf("%s%s%s%s%d", pmgr.config.HaproxyServerNamePrefix, pmgr.config.HaproxyServerNameHostPortSeparator, sanitizer.Replace(host.Host), pmgr.config.HaproxyServerNameHostPortSeparator, host.Port)
}

func setupGlobalProxyManager() (bool, ProxyManager) {
	var GlobalProxyManager ProxyManager
	var upstreamUpdateAPIEnabled bool
	// Conditionally initialize runtime API manager at startup
	if config.ProxyPlatform == "nginx" && len(config.Nginxplusapiaddr) > 0 {
		logger.Debug("Platform: " + config.ProxyPlatform + " API addr: " + config.Nginxplusapiaddr)
		mgr, err := NewNginxAPIManager(
			config.Nginxplusapiaddr,
			time.Duration(config.apiTimeout)*time.Second,
			config.NginxMaxFailsUpstream,
			config.NginxFailTimeoutUpstream,
			config.NginxSlowStartUpstream,
		)
		if err != nil {
			logger.WithFields(logrus.Fields{
				"error": err.Error(),
			}).Fatal("unable to create nginx api manager at startup; exiting nixy")
		}
		GlobalProxyManager = &NginxProxyManager{config: &config, apiManager: mgr}
		upstreamUpdateAPIEnabled = true
		logger.Info("Nginx http api client initialized at startup")
	} else if config.ProxyPlatform == "haproxy" && len(config.HaproxySocketAddr) > 0 {
		logger.Debug("Platform: " + config.ProxyPlatform + " Socket addr: " + config.HaproxySocketAddr)
		logger.Debug("Runtime API add server default-server attributes:" + config.HaproxyAddServerAttributesString)
		logger.Debug("Runtime API add server ssl attributes:" + config.HaproxyAddServerSSLAttributesString)
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(config.apiTimeout)*time.Second)
		defer cancel()
		mgr, err := NewHaproxyManager(ctx, config.HaproxySocketAddr, config.HaproxyDisableLargeBackendCountOptimisation, config.HaproxyAddServerAttributesString, config.HaproxyAddServerSSLAttributesString)
		if err != nil {
			logger.WithFields(logrus.Fields{
				"error": err.Error(),
			}).Fatal("unable to create haproxy manager at startup; exiting nixy")
		}
		GlobalProxyManager = &HAProxyManager{config: &config, apiManager: mgr}
		upstreamUpdateAPIEnabled = true
		logger.Info("Haproxy Runtime API client initialized at startup")
	} else {
		logger.Info("No runtime API based upstream updates enabled at startup")
		return false, nil
	}
	return upstreamUpdateAPIEnabled, GlobalProxyManager
}

// runCommand executes a command and returns a formatted error if it fails.
func runCommand(head string, args ...string) error {
	cmd := exec.Command(head, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		msg := fmt.Sprint(err) + ": " + stderr.String()
		return errors.New(msg)
	}
	return nil
}
