package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/btnmasher/rex/internal/notifications"
	"go.yaml.in/yaml/v3"
)

const (
	currentConfigVersion = 1
	maxConfigBytes       = 1 << 20
	maxStructureTypeIDs  = 128
	maxCorporationIDs    = 128
	allAlertCategories   = "all"
	allAlertWildcard     = "*"
)

// AlertDestination contains shared filters, presentation preferences, and one
// provider-specific delivery configuration.
type AlertDestination struct {
	Name         string
	Filters      AlertFilters
	Presentation Presentation
	Delivery     DeliveryConfig
}

// AlertFilters contains the provider-neutral predicates used to select alerts.
type AlertFilters struct {
	AlertTypes              []string `json:"alertTypes"`
	ExcludeAlertTypes       []string `json:"excludeAlertTypes,omitempty"`
	ExcludeStructureTypeIDs []string `json:"excludeStructureTypeIDs,omitempty"`
	IncludeCorporationIDs   []string `json:"includeCorporationIDs,omitempty"`
	ExcludeCorporationIDs   []string `json:"excludeCorporationIDs,omitempty"`
}

// Presentation contains provider-neutral preferences for rendered alerts.
type Presentation struct {
	ShowEntityIDs bool `json:"showEntityIDs"`
}

// DeliveryConfig identifies a provider and contains its flat provider-specific
// configuration for validation and construction by the provider registry.
type DeliveryConfig struct {
	Type   string
	Fields json.RawMessage
}

// Decode validates and decodes the provider-specific delivery object into a
// provider-owned configuration value.
func (d DeliveryConfig) Decode(target any) error {
	if target == nil {
		return errors.New("delivery decode target is required")
	}
	return decodeStrict(d.Fields, target)
}

type configDocument struct {
	Version      *int            `json:"version"`
	Destinations json.RawMessage `json:"destinations"`
}

type rawDestination struct {
	Name         string          `json:"name"`
	Filters      json.RawMessage `json:"filters"`
	Presentation json.RawMessage `json:"presentation"`
	Delivery     json.RawMessage `json:"delivery"`
}

type deliveryHeader struct {
	Type string `json:"type"`
}

func parseAlertDestinations(path string) ([]AlertDestination, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("ALERT_DESTINATIONS_CONFIG is required")
	}
	raw, err := readConfigFile(path)
	if err != nil {
		return nil, err
	}
	normalized, err := normalizeConfig(path, raw)
	if err != nil {
		return nil, err
	}
	document, err := decodeDocument(normalized)
	if err != nil {
		return nil, fmt.Errorf("ALERT_DESTINATIONS_CONFIG %q: %w", path, err)
	}
	if document.Version == nil || *document.Version != currentConfigVersion {
		return nil, fmt.Errorf("ALERT_DESTINATIONS_CONFIG %q has unsupported version; want %d", path, currentConfigVersion)
	}
	if len(document.Destinations) == 0 || string(document.Destinations) == "null" {
		return nil, fmt.Errorf("ALERT_DESTINATIONS_CONFIG %q must contain destinations", path)
	}
	var rawDestinations []json.RawMessage
	if err := json.Unmarshal(document.Destinations, &rawDestinations); err != nil {
		return nil, fmt.Errorf("ALERT_DESTINATIONS_CONFIG %q destinations must be an array: %w", path, err)
	}
	if len(rawDestinations) == 0 {
		return nil, fmt.Errorf("ALERT_DESTINATIONS_CONFIG %q must contain at least one destination", path)
	}

	destinations := make([]AlertDestination, 0, len(rawDestinations))
	seenNames := make(map[string]struct{}, len(rawDestinations))
	for index, rawDestinationValue := range rawDestinations {
		destination, err := decodeDestination(rawDestinationValue)
		if err != nil {
			return nil, fmt.Errorf("ALERT_DESTINATIONS_CONFIG %q destination %d: %w", path, index, err)
		}
		if err := validateDestination(&destination, seenNames); err != nil {
			return nil, fmt.Errorf("ALERT_DESTINATIONS_CONFIG %q destination %d: %w", path, index, err)
		}
		seenNames[destination.Name] = struct{}{}
		destinations = append(destinations, destination)
	}
	return destinations, nil
}

func readConfigFile(path string) ([]byte, error) {
	file, err := os.Open(path) //nolint:gosec // path is operator-configured
	if err != nil {
		return nil, fmt.Errorf("read ALERT_DESTINATIONS_CONFIG %q: %w", path, err)
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	closeErr := file.Close()
	if err != nil {
		return nil, fmt.Errorf("read ALERT_DESTINATIONS_CONFIG %q: %w", path, err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close ALERT_DESTINATIONS_CONFIG %q: %w", path, closeErr)
	}
	if len(raw) > maxConfigBytes {
		return nil, fmt.Errorf("ALERT_DESTINATIONS_CONFIG %q exceeds %d bytes", path, maxConfigBytes)
	}
	return raw, nil
}

func normalizeConfig(path string, raw []byte) ([]byte, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return raw, nil
	case ".yaml", ".yml":
		return normalizeYAML(raw)
	default:
		return nil, fmt.Errorf("ALERT_DESTINATIONS_CONFIG %q must use .json, .yaml, or .yml", path)
	}
}

func normalizeYAML(raw []byte) ([]byte, error) {
	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("YAML must contain exactly one document")
		}
		return nil, fmt.Errorf("read YAML document: %w", err)
	}
	if err := validateYAMLNode(&document); err != nil {
		return nil, err
	}
	var value any
	if err := document.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode YAML: %w", err)
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("normalize YAML: %w", err)
	}
	return normalized, nil
}

func validateYAMLNode(node *yaml.Node) error {
	if node == nil {
		return errors.New("YAML document is required")
	}
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) != 1 {
			return errors.New("YAML document must contain one value")
		}
		return validateYAMLNode(node.Content[0])
	}
	if node.Kind == yaml.AliasNode {
		return errors.New("YAML aliases and merge keys are not supported")
	}
	if node.Kind == yaml.ScalarNode {
		switch node.Tag {
		case "!!null", "!!bool", "!!int", "!!float", "!!str":
			return nil
		default:
			return fmt.Errorf("YAML tag %q is not supported", node.Tag)
		}
	}
	switch node.Kind {
	case yaml.SequenceNode:
		return validateYAMLSequence(node)
	case yaml.MappingNode:
		return validateYAMLMapping(node)
	default:
		return fmt.Errorf("YAML node kind %d is not supported", node.Kind)
	}
}

func validateYAMLSequence(node *yaml.Node) error {
	for _, child := range node.Content {
		if err := validateYAMLNode(child); err != nil {
			return err
		}
	}
	return nil
}

func validateYAMLMapping(node *yaml.Node) error {
	const mappingPairSize = 2
	seenKeys := make(map[string]struct{}, len(node.Content)/mappingPairSize)
	for index := 0; index < len(node.Content); index += mappingPairSize {
		key := node.Content[index]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
			return errors.New("YAML mapping keys must be strings")
		}
		if _, exists := seenKeys[key.Value]; exists {
			return fmt.Errorf("YAML contains duplicate key %q", key.Value)
		}
		seenKeys[key.Value] = struct{}{}
		if key.Value == "<<" {
			return errors.New("YAML merge keys are not supported")
		}
		if err := validateYAMLNode(node.Content[index+1]); err != nil {
			return err
		}
	}
	return nil
}

func decodeDocument(raw []byte) (configDocument, error) {
	var document configDocument
	if err := decodeStrict(raw, &document); err != nil {
		return configDocument{}, fmt.Errorf("invalid document: %w", err)
	}
	return document, nil
}

func decodeDestination(raw []byte) (AlertDestination, error) {
	var source rawDestination
	if err := decodeStrict(raw, &source); err != nil {
		return AlertDestination{}, err
	}
	if strings.TrimSpace(source.Name) == "" {
		return AlertDestination{}, errors.New("name is required")
	}
	if len(source.Filters) == 0 || string(source.Filters) == "null" {
		return AlertDestination{}, errors.New("filters are required")
	}
	var filters AlertFilters
	if err := decodeStrict(source.Filters, &filters); err != nil {
		return AlertDestination{}, fmt.Errorf("filters: %w", err)
	}
	presentation := Presentation{}
	if len(source.Presentation) > 0 {
		if string(source.Presentation) == "null" {
			return AlertDestination{}, errors.New("presentation must be an object")
		}
		if err := decodeStrict(source.Presentation, &presentation); err != nil {
			return AlertDestination{}, fmt.Errorf("presentation: %w", err)
		}
	}
	if len(source.Delivery) == 0 || string(source.Delivery) == "null" {
		return AlertDestination{}, errors.New("delivery is required")
	}
	var header deliveryHeader
	if err := json.Unmarshal(source.Delivery, &header); err != nil {
		return AlertDestination{}, fmt.Errorf("delivery: %w", err)
	}
	if strings.TrimSpace(header.Type) == "" {
		return AlertDestination{}, errors.New("delivery.type is required")
	}
	return AlertDestination{
		Name:         strings.TrimSpace(source.Name),
		Filters:      filters,
		Presentation: presentation,
		Delivery: DeliveryConfig{
			Type:   strings.ToLower(strings.TrimSpace(header.Type)),
			Fields: append(json.RawMessage(nil), source.Delivery...),
		},
	}, nil
}

func decodeStrict(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func validateDestination(destination *AlertDestination, seenNames map[string]struct{}) error {
	if destination == nil {
		return errors.New("destination is required")
	}
	if _, exists := seenNames[destination.Name]; exists {
		return fmt.Errorf("duplicate destination name %q", destination.Name)
	}
	if err := validateAlertFilters(destination.Name, &destination.Filters); err != nil {
		return err
	}
	return nil
}

func validateAlertFilters(destinationName string, filters *AlertFilters) error {
	if filters == nil || len(filters.AlertTypes) == 0 {
		return fmt.Errorf("destination %q has no alert types", destinationName)
	}
	if err := validateIncludedAlertTypes(destinationName, filters.AlertTypes); err != nil {
		return err
	}
	if err := validateExcludedAlertTypes(destinationName, filters.ExcludeAlertTypes); err != nil {
		return err
	}
	if err := validateNumericIDs(destinationName, "excludeStructureTypeIDs", filters.ExcludeStructureTypeIDs, maxStructureTypeIDs); err != nil {
		return err
	}
	if err := validateNumericIDs(destinationName, "includeCorporationIDs", filters.IncludeCorporationIDs, maxCorporationIDs); err != nil {
		return err
	}
	return validateNumericIDs(destinationName, "excludeCorporationIDs", filters.ExcludeCorporationIDs, maxCorporationIDs)
}

func validateIncludedAlertTypes(destinationName string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for index := range values {
		value := strings.ToLower(strings.TrimSpace(values[index]))
		if value == "" {
			return fmt.Errorf("destination %q contains an empty alert type", destinationName)
		}
		if value == allAlertCategories || value == allAlertWildcard {
			if len(values) != 1 {
				return fmt.Errorf("destination %q cannot combine %q with other alert types", destinationName, value)
			}
			values[index] = value
			continue
		}
		canonical, err := notifications.NormalizeAlertSelector(value)
		if err != nil {
			return fmt.Errorf("destination %q has invalid alert type %q: %w", destinationName, value, err)
		}
		if _, exists := seen[canonical]; exists {
			return fmt.Errorf("destination %q contains duplicate alert type %q", destinationName, canonical)
		}
		seen[canonical] = struct{}{}
		values[index] = canonical
	}
	return nil
}

func validateExcludedAlertTypes(destinationName string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for index := range values {
		value := strings.ToLower(strings.TrimSpace(values[index]))
		if value == "" {
			return fmt.Errorf("destination %q contains an empty excluded alert type", destinationName)
		}
		if value == allAlertCategories || value == allAlertWildcard {
			return fmt.Errorf("destination %q can exclude only individual alert types, got %q", destinationName, value)
		}
		canonical, err := notifications.NormalizeAlertSelector(value)
		if err != nil {
			return fmt.Errorf("destination %q has invalid excluded alert type %q: %w", destinationName, value, err)
		}
		if notifications.IsAlertGroup(canonical) {
			return fmt.Errorf("destination %q can exclude only individual alert types, got %q", destinationName, value)
		}
		if _, exists := seen[canonical]; exists {
			return fmt.Errorf("destination %q contains duplicate excluded alert type %q", destinationName, canonical)
		}
		seen[canonical] = struct{}{}
		values[index] = canonical
	}
	return nil
}

func validateNumericIDs(destinationName, fieldName string, values []string, limit int) error {
	if len(values) > limit {
		return fmt.Errorf("destination %q has more than %d %s", destinationName, limit, fieldName)
	}
	seen := make(map[string]struct{}, len(values))
	for index := range values {
		value := strings.TrimSpace(values[index])
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil || parsed == 0 {
			return fmt.Errorf("destination %q has invalid %s value %q", destinationName, fieldName, value)
		}
		value = strconv.FormatUint(parsed, 10)
		if _, exists := seen[value]; exists {
			return fmt.Errorf("destination %q contains duplicate %s value %q", destinationName, fieldName, value)
		}
		seen[value] = struct{}{}
		values[index] = value
	}
	return nil
}
