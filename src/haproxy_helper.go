package main

import (
	"fmt"
	"strconv"
	"strings"

	runtime_misc "github.com/haproxytech/client-native/v5/misc"
	runtime_models "github.com/haproxytech/client-native/v5/models"
	runtime_api "github.com/haproxytech/client-native/v5/runtime"
)

func haproxyClientExecuteWithResponse(command string, client runtime_api.Runtime) (string, error) {
	rawdata, err := client.ExecuteRaw(command)
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

func haproxyClientGetServersStateWithBackend(client runtime_api.Runtime) (map[string]runtime_models.RuntimeServers, error) {
	cmd := fmt.Sprintf("show servers state")
	result, err := haproxyClientExecuteWithResponse(cmd, client)
	if err != nil {
		return nil, err
	}
	return haproxtClientParseRuntimeServersWithBackend(result)
}

func haproxtClientParseRuntimeServersWithBackend(output string) (map[string]runtime_models.RuntimeServers, error) {
	lines := strings.Split(output, "\n")
	result := make(map[string]runtime_models.RuntimeServers)

	if strings.TrimSpace(lines[0]) != "1" {
		return nil, fmt.Errorf("unsupported output format version, supporting format version 1")
	}
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "1" {
			continue
		}
		backend, server := haproxyClientParseRuntimeServerWithBackend(line)
		if server != nil && backend != "" {
			if _, ok := result[backend]; !ok {
				result[backend] = []*runtime_models.RuntimeServer{}
			}
			result[backend] = append(result[backend], server)
		}
	}
	return result, nil
}

func haproxyClientParseRuntimeServerWithBackend(line string) (backend string, server *runtime_models.RuntimeServer) {
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
