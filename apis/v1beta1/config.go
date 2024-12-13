// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v1beta1

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"dario.cat/mergo"
	"github.com/go-logr/logr"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/open-telemetry/opentelemetry-operator/internal/components"
	"github.com/open-telemetry/opentelemetry-operator/internal/components/exporters"
	"github.com/open-telemetry/opentelemetry-operator/internal/components/extensions"
	"github.com/open-telemetry/opentelemetry-operator/internal/components/processors"
	"github.com/open-telemetry/opentelemetry-operator/internal/components/receivers"
)

type ComponentKind int

const (
	KindReceiver ComponentKind = iota
	KindExporter
	KindProcessor
	KindExtension
)

func (c ComponentKind) String() string {
	return [...]string{"receiver", "exporter", "processor", "extension"}[c]
}

// AnyConfig represent parts of the config.
type AnyConfig struct {
	Object map[string]interface{} `json:"-" yaml:",inline"`
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (c *AnyConfig) DeepCopyInto(out *AnyConfig) {
	*out = *c
	if c.Object != nil {
		in, out := &c.Object, &out.Object
		*out = make(map[string]interface{}, len(*in))
		for key, val := range *in {
			(*out)[key] = val
		}
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new AnyConfig.
func (c *AnyConfig) DeepCopy() *AnyConfig {
	if c == nil {
		return nil
	}
	out := new(AnyConfig)
	c.DeepCopyInto(out)
	return out
}

var _ json.Marshaler = &AnyConfig{}
var _ json.Unmarshaler = &AnyConfig{}

// UnmarshalJSON implements an alternative parser for this field.
func (c *AnyConfig) UnmarshalJSON(b []byte) error {
	vals := map[string]interface{}{}
	if err := json.Unmarshal(b, &vals); err != nil {
		return err
	}
	c.Object = vals
	return nil
}

// MarshalJSON specifies how to convert this object into JSON.
func (c *AnyConfig) MarshalJSON() ([]byte, error) {
	if c == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(c.Object)
}

// Pipeline is a struct of component type to a list of component IDs.
type Pipeline struct {
	Exporters  []string `json:"exporters" yaml:"exporters"`
	Processors []string `json:"processors,omitempty" yaml:"processors,omitempty"`
	Receivers  []string `json:"receivers" yaml:"receivers"`
}

// GetEnabledComponents constructs a list of enabled components by component type.
func (c *Config) GetEnabledComponents() map[ComponentKind]map[string]interface{} {
	toReturn := map[ComponentKind]map[string]interface{}{
		KindReceiver:  {},
		KindProcessor: {},
		KindExporter:  {},
		KindExtension: {},
	}
	for _, extension := range c.Service.Extensions {
		toReturn[KindExtension][extension] = struct{}{}
	}

	for _, pipeline := range c.Service.Pipelines {
		if pipeline == nil {
			continue
		}
		for _, componentId := range pipeline.Receivers {
			toReturn[KindReceiver][componentId] = struct{}{}
		}
		for _, componentId := range pipeline.Exporters {
			toReturn[KindExporter][componentId] = struct{}{}
		}
		for _, componentId := range pipeline.Processors {
			toReturn[KindProcessor][componentId] = struct{}{}
		}
	}
	for _, componentId := range c.Service.Extensions {
		toReturn[KindExtension][componentId] = struct{}{}
	}
	return toReturn
}

// Config encapsulates collector config.
type Config struct {
	// +kubebuilder:pruning:PreserveUnknownFields
	Receivers AnyConfig `json:"receivers" yaml:"receivers"`
	// +kubebuilder:pruning:PreserveUnknownFields
	Exporters AnyConfig `json:"exporters" yaml:"exporters"`
	// +kubebuilder:pruning:PreserveUnknownFields
	Processors *AnyConfig `json:"processors,omitempty" yaml:"processors,omitempty"`
	// +kubebuilder:pruning:PreserveUnknownFields
	Connectors *AnyConfig `json:"connectors,omitempty" yaml:"connectors,omitempty"`
	// +kubebuilder:pruning:PreserveUnknownFields
	Extensions *AnyConfig `json:"extensions,omitempty" yaml:"extensions,omitempty"`
	Service    Service    `json:"service" yaml:"service"`
}

// getRbacRulesForComponentKinds gets the RBAC Rules for the given ComponentKind(s).
func (c *Config) getRbacRulesForComponentKinds(logger logr.Logger, componentKinds ...ComponentKind) ([]rbacv1.PolicyRule, error) {
	var rules []rbacv1.PolicyRule
	enabledComponents := c.GetEnabledComponents()
	for _, componentKind := range componentKinds {
		var retriever components.ParserRetriever
		var cfg AnyConfig
		switch componentKind {
		case KindReceiver:
			retriever = receivers.ReceiverFor
			cfg = c.Receivers
		case KindExporter:
			retriever = exporters.ParserFor
			cfg = c.Exporters
		case KindProcessor:
			retriever = processors.ProcessorFor
			if c.Processors == nil {
				cfg = AnyConfig{}
			} else {
				cfg = *c.Processors
			}
		case KindExtension:
			continue
		}
		for componentName := range enabledComponents[componentKind] {
			// TODO: Clean up the naming here and make it simpler to use a retriever.
			parser := retriever(componentName)
			if parsedRules, err := parser.GetRBACRules(logger, cfg.Object[componentName]); err != nil {
				return nil, err
			} else {
				rules = append(rules, parsedRules...)
			}
		}
	}
	return rules, nil
}

// getPortsForComponentKinds gets the ports for the given ComponentKind(s).
func (c *Config) getPortsForComponentKinds(logger logr.Logger, componentKinds ...ComponentKind) ([]corev1.ServicePort, error) {
	var ports []corev1.ServicePort
	enabledComponents := c.GetEnabledComponents()
	for _, componentKind := range componentKinds {
		var retriever components.ParserRetriever
		var cfg AnyConfig
		switch componentKind {
		case KindReceiver:
			retriever = receivers.ReceiverFor
			cfg = c.Receivers
		case KindExporter:
			retriever = exporters.ParserFor
			cfg = c.Exporters
		case KindProcessor:
			continue
		case KindExtension:
			retriever = extensions.ParserFor
			if c.Extensions == nil {
				cfg = AnyConfig{}
			} else {
				cfg = *c.Extensions
			}
		}
		for componentName := range enabledComponents[componentKind] {
			// TODO: Clean up the naming here and make it simpler to use a retriever.
			parser := retriever(componentName)
			if parsedPorts, err := parser.Ports(logger, componentName, cfg.Object[componentName]); err != nil {
				return nil, err
			} else {
				ports = append(ports, parsedPorts...)
			}
		}
	}

	sort.Slice(ports, func(i, j int) bool {
		return ports[i].Name < ports[j].Name
	})

	return ports, nil
}

// getEnvironmentVariablesForComponentKinds gets the environment variables for the given ComponentKind(s).
func (c *Config) getEnvironmentVariablesForComponentKinds(logger logr.Logger, componentKinds ...ComponentKind) ([]corev1.EnvVar, error) {
	var envVars []corev1.EnvVar = []corev1.EnvVar{}
	enabledComponents := c.GetEnabledComponents()
	for _, componentKind := range componentKinds {
		var retriever components.ParserRetriever
		var cfg AnyConfig

		switch componentKind {
		case KindReceiver:
			retriever = receivers.ReceiverFor
			cfg = c.Receivers
		case KindExporter:
			continue
		case KindProcessor:
			continue
		case KindExtension:
			continue
		}
		for componentName := range enabledComponents[componentKind] {
			parser := retriever(componentName)
			if parsedEnvVars, err := parser.GetEnvironmentVariables(logger, cfg.Object[componentName]); err != nil {
				return nil, err
			} else {
				envVars = append(envVars, parsedEnvVars...)
			}
		}
	}

	sort.Slice(envVars, func(i, j int) bool {
		return envVars[i].Name < envVars[j].Name
	})

	return envVars, nil
}

// applyDefaultForComponentKinds applies defaults to the endpoints for the given ComponentKind(s).
func (c *Config) applyDefaultForComponentKinds(logger logr.Logger, componentKinds ...ComponentKind) error {
	if err := c.Service.ApplyDefaults(logger); err != nil {
		return err
	}
	enabledComponents := c.GetEnabledComponents()
	for _, componentKind := range componentKinds {
		var retriever components.ParserRetriever
		var cfg AnyConfig
		switch componentKind {
		case KindReceiver:
			retriever = receivers.ReceiverFor
			cfg = c.Receivers
		case KindExporter:
			continue
		case KindProcessor:
			continue
		case KindExtension:
			continue
		}
		for componentName := range enabledComponents[componentKind] {
			parser := retriever(componentName)
			componentConf := cfg.Object[componentName]
			newCfg, err := parser.GetDefaultConfig(logger, componentConf)
			if err != nil {
				return err
			}

			// We need to ensure we don't remove any fields in defaulting.
			mappedCfg, ok := newCfg.(map[string]interface{})
			if !ok || mappedCfg == nil {
				logger.V(1).Info("returned default configuration invalid",
					"warn", "could not apply component defaults",
					"component", componentName,
				)
				continue
			}

			if err := mergo.Merge(&mappedCfg, componentConf); err != nil {
				return err
			}
			cfg.Object[componentName] = mappedCfg
		}
	}

	return nil
}

func (c *Config) GetReceiverPorts(logger logr.Logger) ([]corev1.ServicePort, error) {
	return c.getPortsForComponentKinds(logger, KindReceiver)
}

func (c *Config) GetExporterPorts(logger logr.Logger) ([]corev1.ServicePort, error) {
	return c.getPortsForComponentKinds(logger, KindExporter)
}

func (c *Config) GetExtensionPorts(logger logr.Logger) ([]corev1.ServicePort, error) {
	return c.getPortsForComponentKinds(logger, KindExtension)
}

func (c *Config) GetReceiverAndExporterPorts(logger logr.Logger) ([]corev1.ServicePort, error) {
	return c.getPortsForComponentKinds(logger, KindReceiver, KindExporter)
}

func (c *Config) GetAllPorts(logger logr.Logger) ([]corev1.ServicePort, error) {
	return c.getPortsForComponentKinds(logger, KindReceiver, KindExporter, KindExtension)
}

func (c *Config) GetEnvironmentVariables(logger logr.Logger) ([]corev1.EnvVar, error) {
	return c.getEnvironmentVariablesForComponentKinds(logger, KindReceiver)
}

func (c *Config) GetAllRbacRules(logger logr.Logger) ([]rbacv1.PolicyRule, error) {
	return c.getRbacRulesForComponentKinds(logger, KindReceiver, KindExporter, KindProcessor)
}

func (c *Config) ApplyDefaults(logger logr.Logger) error {
	return c.applyDefaultForComponentKinds(logger, KindReceiver)
}

// GetLivenessProbe gets the first enabled liveness probe. There should only ever be one extension enabled
// that provides the hinting for the liveness probe.
func (c *Config) GetLivenessProbe(logger logr.Logger) (*corev1.Probe, error) {
	enabledComponents := c.GetEnabledComponents()
	for componentName := range enabledComponents[KindExtension] {
		// TODO: Clean up the naming here and make it simpler to use a retriever.
		parser := extensions.ParserFor(componentName)
		if probe, err := parser.GetLivenessProbe(logger, c.Extensions.Object[componentName]); err != nil {
			return nil, err
		} else if probe != nil {
			return probe, nil
		}
	}
	return nil, nil
}

// GetReadinessProbe gets the first enabled readiness probe. There should only ever be one extension enabled
// that provides the hinting for the readiness probe.
func (c *Config) GetReadinessProbe(logger logr.Logger) (*corev1.Probe, error) {
	enabledComponents := c.GetEnabledComponents()
	for componentName := range enabledComponents[KindExtension] {
		// TODO: Clean up the naming here and make it simpler to use a retriever.
		parser := extensions.ParserFor(componentName)
		if probe, err := parser.GetReadinessProbe(logger, c.Extensions.Object[componentName]); err != nil {
			return nil, err
		} else if probe != nil {
			return probe, nil
		}
	}
	return nil, nil
}

// Yaml encodes the current object and returns it as a string.
func (c *Config) Yaml() (string, error) {
	var buf bytes.Buffer
	yamlEncoder := yaml.NewEncoder(&buf)
	yamlEncoder.SetIndent(2)
	if err := yamlEncoder.Encode(&c); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// Returns null objects in the config.
func (c *Config) nullObjects() []string {
	var nullKeys []string
	if nulls := hasNullValue(c.Receivers.Object); len(nulls) > 0 {
		nullKeys = append(nullKeys, addPrefix("receivers.", nulls)...)
	}
	if nulls := hasNullValue(c.Exporters.Object); len(nulls) > 0 {
		nullKeys = append(nullKeys, addPrefix("exporters.", nulls)...)
	}
	if c.Processors != nil {
		if nulls := hasNullValue(c.Processors.Object); len(nulls) > 0 {
			nullKeys = append(nullKeys, addPrefix("processors.", nulls)...)
		}
	}
	if c.Extensions != nil {
		if nulls := hasNullValue(c.Extensions.Object); len(nulls) > 0 {
			nullKeys = append(nullKeys, addPrefix("extensions.", nulls)...)
		}
	}
	if c.Connectors != nil {
		if nulls := hasNullValue(c.Connectors.Object); len(nulls) > 0 {
			nullKeys = append(nullKeys, addPrefix("connectors.", nulls)...)
		}
	}
	// Make the return deterministic. The config uses maps therefore processing order is non-deterministic.
	sort.Strings(nullKeys)
	return nullKeys
}

type Service struct {
	Extensions []string `json:"extensions,omitempty" yaml:"extensions,omitempty"`
	// +kubebuilder:pruning:PreserveUnknownFields
	Telemetry *AnyConfig `json:"telemetry,omitempty" yaml:"telemetry,omitempty"`
	// +kubebuilder:pruning:PreserveUnknownFields
	Pipelines map[string]*Pipeline `json:"pipelines" yaml:"pipelines"`
}

const (
	defaultServicePort int32 = 8888
	defaultServiceHost       = "0.0.0.0"
)

// MetricsEndpoint attempts gets the host and port number from the host address without doing any validation regarding the
// address itself.
// It works even before env var expansion happens, when a simple `net.SplitHostPort` would fail because of the extra colon
// from the env var, i.e. the address looks like "${env:POD_IP}:4317", "${env:POD_IP}", or "${POD_IP}".
// In cases which the port itself is a variable, i.e. "${env:POD_IP}:${env:PORT}", this returns an error. This happens
// because the port is used to generate Service objects and mappings.
func (s *Service) MetricsEndpoint(logger logr.Logger) (string, int32, error) {
	telemetry := s.GetTelemetry()
	if telemetry == nil || telemetry.Metrics.Address == "" {
		return defaultServiceHost, defaultServicePort, nil
	}

	// The regex below matches on strings that end with a colon followed by the environment variable expansion syntax.
	// So it should match on strings ending with: ":${env:POD_IP}" or ":${POD_IP}".
	const portEnvVarRegex = `:\${[env:]?.*}$`
	isPortEnvVar := regexp.MustCompile(portEnvVarRegex).MatchString(telemetry.Metrics.Address)
	if isPortEnvVar {
		errMsg := fmt.Sprintf("couldn't determine metrics port from configuration: %s",
			telemetry.Metrics.Address)
		logger.Info(errMsg)
		return "", 0, fmt.Errorf(errMsg)
	}

	// The regex below matches on strings that end with a colon followed by 1 or more numbers (representing the port).
	const explicitPortRegex = `:(\d+$)`
	explicitPortMatches := regexp.MustCompile(explicitPortRegex).FindStringSubmatch(telemetry.Metrics.Address)
	if len(explicitPortMatches) <= 1 {
		return telemetry.Metrics.Address, defaultServicePort, nil
	}

	port, err := strconv.ParseInt(explicitPortMatches[1], 10, 32)
	if err != nil {
		errMsg := fmt.Sprintf("couldn't determine metrics port from configuration: %s",
			telemetry.Metrics.Address)
		logger.Info(errMsg, "error", err)
		return "", 0, err
	}

	host, _, _ := strings.Cut(telemetry.Metrics.Address, explicitPortMatches[0])
	return host, int32(port), nil
}

// ApplyDefaults inserts configuration defaults if it has not been set.
func (s *Service) ApplyDefaults(logger logr.Logger) error {
	telemetryAddr, telemetryPort, err := s.MetricsEndpoint(logger)
	if err != nil {
		return err
	}

	tm := &AnyConfig{
		Object: map[string]interface{}{
			"metrics": map[string]interface{}{
				"address": fmt.Sprintf("%s:%d", telemetryAddr, telemetryPort),
			},
		},
	}

	if s.Telemetry == nil {
		s.Telemetry = tm
		return nil
	}
	// NOTE: Merge without overwrite. If a telemetry endpoint is specified, the defaulting
	// respects the configuration and returns an equal value.
	if err := mergo.Merge(s.Telemetry, tm); err != nil {
		return fmt.Errorf("telemetry config merge failed: %w", err)
	}
	return nil
}

// MetricsConfig comes from the collector.
type MetricsConfig struct {
	// Level is the level of telemetry metrics, the possible values are:
	//  - "none" indicates that no telemetry data should be collected;
	//  - "basic" is the recommended and covers the basics of the service telemetry.
	//  - "normal" adds some other indicators on top of basic.
	//  - "detailed" adds dimensions and views to the previous levels.
	Level string `json:"level,omitempty" yaml:"level,omitempty"`

	// Address is the [address]:port that metrics exposition should be bound to.
	Address string `json:"address,omitempty" yaml:"address,omitempty"`
}

// Telemetry is an intermediary type that allows for easy access to the collector's telemetry settings.
type Telemetry struct {
	Metrics MetricsConfig `json:"metrics,omitempty" yaml:"metrics,omitempty"`

	// Resource specifies user-defined attributes to include with all emitted telemetry.
	// Note that some attributes are added automatically (e.g. service.version) even
	// if they are not specified here. In order to suppress such attributes the
	// attribute must be specified in this map with null YAML value (nil string pointer).
	Resource map[string]*string `json:"resource,omitempty" yaml:"resource,omitempty"`
}

// GetTelemetry serves as a helper function to access the fields we care about in the underlying telemetry struct.
// This exists to avoid needing to worry extra fields in the telemetry struct.
func (s *Service) GetTelemetry() *Telemetry {
	if s.Telemetry == nil {
		return nil
	}
	// Convert map to JSON bytes
	jsonData, err := json.Marshal(s.Telemetry)
	if err != nil {
		return nil
	}
	t := &Telemetry{}
	// Unmarshal JSON into the provided struct
	if err := json.Unmarshal(jsonData, t); err != nil {
		return nil
	}
	return t
}

func hasNullValue(cfg map[string]interface{}) []string {
	var nullKeys []string
	for k, v := range cfg {
		if v == nil {
			nullKeys = append(nullKeys, fmt.Sprintf("%s:", k))
		}
		if reflect.ValueOf(v).Kind() == reflect.Map {
			var nulls []string
			val, ok := v.(map[string]interface{})
			if ok {
				nulls = hasNullValue(val)
			}
			if len(nulls) > 0 {
				prefixed := addPrefix(k+".", nulls)
				nullKeys = append(nullKeys, prefixed...)
			}
		}
	}
	return nullKeys
}

func addPrefix(prefix string, arr []string) []string {
	var prefixed []string
	for _, v := range arr {
		prefixed = append(prefixed, fmt.Sprintf("%s%s", prefix, v))
	}
	return prefixed
}
