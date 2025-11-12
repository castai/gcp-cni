package installer

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

type CNIConfig struct {
	CNIVersion string                   `json:"cniVersion"`
	Name       string                   `json:"name"`
	Plugins    []map[string]interface{} `json:"plugins"`
}

func UpdateCNIIPAM(data []byte, ipamType string, logger *slog.Logger) ([]byte, error) {
	var config CNIConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse CNI config: %w", err)
	}

	modified := false
	for i, plugin := range config.Plugins {
		ipam, ok := plugin["ipam"].(map[string]interface{})
		if !ok {
			continue
		}

		ipam["type"] = ipamType
		config.Plugins[i] = plugin
		modified = true
		logger.Info("Updated IPAM plugin to use gcp-ipam IPAM")
	}

	if !modified {
		logger.Warn("No IPAM plugin found in CNI configuration")
		return nil, nil
	}

	updatedData, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal updated config: %w", err)
	}
	return updatedData, nil
}
