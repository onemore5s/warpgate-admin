package config

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type File struct {
	Warpgate  Warpgate  `yaml:"warpgate"`
	Inventory Inventory `yaml:"inventory"`
	Apply     Apply     `yaml:"apply"`
}

type Warpgate struct {
	Host               string `yaml:"host"`
	Token              string `yaml:"token"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
}

type Inventory struct {
	Groups  string `yaml:"groups"`
	Targets string `yaml:"targets"`
}

type Apply struct {
	UpdatePasswords bool `yaml:"update_passwords"`
	SyncRoles       bool `yaml:"sync_roles"`
	SyncTargets     bool `yaml:"sync_targets"`
}

func Parse(input io.Reader) (File, error) {
	decoder := yaml.NewDecoder(input)
	decoder.KnownFields(true)

	var result File
	if err := decoder.Decode(&result); errors.Is(err, io.EOF) {
		return File{}, errors.New("configuration is empty")
	} else if err != nil {
		return File{}, fmt.Errorf("decode YAML: %w", err)
	}

	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return File{}, fmt.Errorf("decode YAML: %w", err)
		}
		return File{}, errors.New("configuration must contain exactly one YAML document")
	}

	if result.Inventory.Groups == "" {
		return File{}, errors.New("inventory.groups is required")
	}
	if result.Inventory.Targets == "" {
		return File{}, errors.New("inventory.targets is required")
	}
	return result, nil
}

func (config File) ResolvePaths(configPath string) File {
	baseDirectory := filepath.Dir(configPath)
	if !filepath.IsAbs(config.Inventory.Groups) {
		config.Inventory.Groups = filepath.Clean(filepath.Join(baseDirectory, config.Inventory.Groups))
	}
	if !filepath.IsAbs(config.Inventory.Targets) {
		config.Inventory.Targets = filepath.Clean(filepath.Join(baseDirectory, config.Inventory.Targets))
	}
	return config
}
