package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const proxyLifecycleRequestTimeout = 5 * time.Second

func notifyProxyLifecycle(event, baseURL string) error {
	endpoint, err := proxyLifecycleEndpoint(event, baseURL)
	if err != nil {
		return err
	}

	request, err := http.NewRequest(http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create proxy lifecycle request: %w", err)
	}

	client := &http.Client{Timeout: proxyLifecycleRequestTimeout}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("notify proxy lifecycle %q: %w", event, err)
	}
	defer response.Body.Close()

	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
	if readErr != nil {
		return fmt.Errorf("read proxy lifecycle response: %w", readErr)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("proxy lifecycle %q returned HTTP %d: %s", event, response.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	return nil
}

func proxyLifecycleEndpoint(event, baseURL string) (string, error) {
	var lifecyclePath string
	switch event {
	case "begin":
		lifecyclePath = "/v1/proxy/restart/begin"
	case "complete":
		lifecyclePath = "/v1/proxy/restart/complete"
	default:
		return "", fmt.Errorf("invalid proxy lifecycle event %q: expected begin or complete", event)
	}

	if baseURL == "" {
		scheme := "http"
		if config.PortWithTLS {
			scheme = "https"
		}
		host := config.Address
		switch host {
		case "", "0.0.0.0":
			host = "127.0.0.1"
		case "::", "[::]":
			host = "::1"
		}
		baseURL = scheme + "://" + net.JoinHostPort(host, config.Port)
	}

	parsedBaseURL, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse proxy lifecycle base URL: %w", err)
	}
	if parsedBaseURL.Scheme != "http" && parsedBaseURL.Scheme != "https" {
		return "", fmt.Errorf("proxy lifecycle base URL must use http or https")
	}
	if parsedBaseURL.Host == "" {
		return "", fmt.Errorf("proxy lifecycle base URL must include a host")
	}
	parsedBaseURL.Path = strings.TrimRight(parsedBaseURL.Path, "/") + lifecyclePath
	parsedBaseURL.RawQuery = ""
	parsedBaseURL.Fragment = ""
	return parsedBaseURL.String(), nil
}
