// Package routing evaluates destination policy before and after enrichment.
package routing

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/btnmasher/rex/internal/authnextdb"
	"github.com/btnmasher/rex/internal/enrichment"
	"github.com/btnmasher/rex/internal/notifications"
)

// Destination describes one logical destination and its opaque delivery targets.
type Destination struct {
	ID        string
	TargetIDs []string
	Filters   Filters
}

// Filters contains the provider-neutral predicates used by a destination.
type Filters struct {
	AlertTypes              []string
	ExcludeAlertTypes       []string
	ExcludeStructureTypeIDs []string
	IncludeCorporationIDs   []string
	ExcludeCorporationIDs   []string
}

type compiledDestination struct {
	destination  Destination
	routing      alertRouting
	corporations corporationFilter
	targets      []string
}

// Policy evaluates configured destination selectors and exclusions.
type Policy struct {
	destinations []compiledDestination
	targetOwners map[string]string
}

// New compiles destination selectors and rejects duplicate target IDs at the
// policy boundary.
func New(destinations []Destination) (*Policy, error) {
	policy := &Policy{
		destinations: make([]compiledDestination, 0, len(destinations)),
		targetOwners: make(map[string]string),
	}
	seenTargets := make(map[string]struct{})
	seenDestinationIDs := make(map[string]struct{}, len(destinations))
	for index := range destinations {
		destination := cloneDestination(&destinations[index])
		destination.ID = strings.TrimSpace(destination.ID)
		if destination.ID == "" {
			return nil, errors.New("destination name is required")
		}
		if _, exists := seenDestinationIDs[destination.ID]; exists {
			return nil, fmt.Errorf("duplicate destination %q", destination.ID)
		}
		seenDestinationIDs[destination.ID] = struct{}{}
		compiled, err := compileDestination(&destination, seenTargets)
		if err != nil {
			return nil, fmt.Errorf("destination %q: %w", destination.ID, err)
		}
		for _, targetID := range compiled.targets {
			policy.targetOwners[targetID] = destination.ID
		}
		policy.destinations = append(policy.destinations, compiled)
	}
	return policy, nil
}

func compileDestination(destination *Destination, seenTargets map[string]struct{}) (compiledDestination, error) {
	if destination == nil {
		return compiledDestination{}, errors.New("destination is required")
	}
	compiledRouting, err := compileAlertRouting(destination.Filters.AlertTypes, destination.Filters.ExcludeAlertTypes)
	if err != nil {
		return compiledDestination{}, err
	}
	compiled := compiledDestination{
		destination:  *destination,
		routing:      compiledRouting,
		corporations: compileCorporationFilter(destination.Filters.IncludeCorporationIDs, destination.Filters.ExcludeCorporationIDs),
	}
	for _, rawTargetID := range destination.TargetIDs {
		targetID := strings.TrimSpace(rawTargetID)
		if targetID == "" {
			continue
		}
		if _, seen := seenTargets[targetID]; seen {
			return compiledDestination{}, fmt.Errorf("target ID %q is assigned more than once", targetID)
		}
		seenTargets[targetID] = struct{}{}
		compiled.targets = append(compiled.targets, targetID)
	}
	return compiled, nil
}

// PreRoute selects targets using only the polling corporation and classified
// alert type. It does not perform enrichment or external I/O.
func (p *Policy) PreRoute(corporationID, alertType string) []string {
	if p == nil {
		return nil
	}
	return p.routeTargets(nil, corporationID, alertType)
}

// Restrict limits pre-routed targets to requested destination or target IDs.
func (p *Policy) Restrict(targetIDs, requested []string) []string {
	if p == nil || len(requested) == 0 {
		return append([]string(nil), targetIDs...)
	}
	selection := make(map[string]struct{}, len(requested))
	for _, value := range requested {
		selection[strings.TrimSpace(value)] = struct{}{}
	}
	filtered := make([]string, 0, len(targetIDs))
	for _, targetID := range targetIDs {
		if _, ok := selection[targetID]; ok {
			filtered = append(filtered, targetID)
			continue
		}
		owner := p.targetOwners[targetID]
		if _, ok := selection[owner]; ok {
			filtered = append(filtered, targetID)
		}
	}
	return filtered
}

// PostRoute applies enrichment-dependent structure-type exclusions to the
// supplied candidate targets.
func (p *Policy) PostRoute(view *enrichment.Context, candidateIDs []string) []string {
	if p == nil || view == nil {
		return nil
	}
	candidates := make(map[string]struct{}, len(candidateIDs))
	for _, candidateID := range candidateIDs {
		candidates[candidateID] = struct{}{}
	}
	selected := make([]string, 0, len(candidateIDs))
	for index := range p.destinations {
		destination := &p.destinations[index]
		if !destination.corporations.supports(view.PollingCorporationID) || !destination.routing.supports(view.Event.AlertType) {
			continue
		}
		if destinationExcludesStructureType(destination, view) {
			continue
		}
		for _, targetID := range destination.targets {
			if _, ok := candidates[targetID]; ok {
				selected = append(selected, targetID)
			}
		}
	}
	return selected
}

func (p *Policy) routeTargets(requested map[string]struct{}, corporationID, alertType string) []string {
	selected := make([]string, 0)
	seen := make(map[string]struct{})
	for index := range p.destinations {
		destination := &p.destinations[index]
		if !destinationSelected(requested, destination) || !destination.corporations.supports(corporationID) || !destination.routing.supports(alertType) {
			continue
		}
		for _, targetID := range destination.targets {
			if _, ok := seen[targetID]; ok {
				continue
			}
			seen[targetID] = struct{}{}
			selected = append(selected, targetID)
		}
	}
	return selected
}

func cloneDestination(destination *Destination) Destination {
	if destination == nil {
		return Destination{}
	}
	clone := *destination
	clone.TargetIDs = append([]string(nil), destination.TargetIDs...)
	clone.Filters.AlertTypes = append([]string(nil), destination.Filters.AlertTypes...)
	clone.Filters.ExcludeAlertTypes = append([]string(nil), destination.Filters.ExcludeAlertTypes...)
	clone.Filters.ExcludeStructureTypeIDs = append([]string(nil), destination.Filters.ExcludeStructureTypeIDs...)
	clone.Filters.IncludeCorporationIDs = append([]string(nil), destination.Filters.IncludeCorporationIDs...)
	clone.Filters.ExcludeCorporationIDs = append([]string(nil), destination.Filters.ExcludeCorporationIDs...)
	return clone
}

func destinationSelected(requested map[string]struct{}, destination *compiledDestination) bool {
	if destination == nil || len(requested) == 0 {
		return destination != nil
	}
	if _, ok := requested[destination.destination.ID]; ok {
		return true
	}
	for _, targetID := range destination.targets {
		if _, ok := requested[targetID]; ok {
			return true
		}
	}
	return false
}

type alertRouting struct {
	included map[string]struct{}
	excluded map[string]struct{}
}

func compileAlertRouting(includedSelectors, excludedSelectors []string) (alertRouting, error) {
	routing := alertRouting{included: make(map[string]struct{}), excluded: make(map[string]struct{})}
	for _, selector := range includedSelectors {
		alertTypes, err := expandIncludedAlertSelector(selector)
		if err != nil {
			return alertRouting{}, fmt.Errorf("include selector %q: %w", selector, err)
		}
		for _, alertType := range alertTypes {
			routing.included[alertType] = struct{}{}
		}
	}
	for _, selector := range excludedSelectors {
		canonical, err := notifications.NormalizeAlertSelector(selector)
		if err != nil {
			return alertRouting{}, fmt.Errorf("exclude selector %q: %w", selector, err)
		}
		alertTypes, err := notifications.ExpandAlertSelector(canonical)
		if err != nil || notifications.IsAlertGroup(canonical) || len(alertTypes) != 1 {
			return alertRouting{}, fmt.Errorf("exclude selector %q is not a leaf", selector)
		}
		routing.excluded[alertTypes[0]] = struct{}{}
	}
	return routing, nil
}

func expandIncludedAlertSelector(selector string) ([]string, error) {
	selector = strings.ToLower(strings.TrimSpace(selector))
	if selector == "all" || selector == "*" {
		return notifications.AllAlertTypes(), nil
	}
	return notifications.ExpandAlertSelector(selector)
}

func (r alertRouting) supports(alertType string) bool {
	if _, excluded := r.excluded[alertType]; excluded {
		return false
	}
	_, included := r.included[alertType]
	return included
}

type corporationFilter struct {
	included map[string]struct{}
	excluded map[string]struct{}
}

func compileCorporationFilter(included, excluded []string) corporationFilter {
	filter := corporationFilter{included: make(map[string]struct{}), excluded: make(map[string]struct{})}
	for _, corporationID := range included {
		if corporationID = strings.TrimSpace(corporationID); corporationID != "" {
			filter.included[corporationID] = struct{}{}
		}
	}
	for _, corporationID := range excluded {
		if corporationID = strings.TrimSpace(corporationID); corporationID != "" {
			filter.excluded[corporationID] = struct{}{}
		}
	}
	return filter
}

func (f corporationFilter) supports(corporationID string) bool {
	corporationID = strings.TrimSpace(corporationID)
	if _, excluded := f.excluded[corporationID]; excluded {
		return false
	}
	if len(f.included) == 0 {
		return true
	}
	_, included := f.included[corporationID]
	return included
}

func destinationExcludesStructureType(destination *compiledDestination, view *enrichment.Context) bool {
	if destination == nil || view == nil || len(destination.destination.Filters.ExcludeStructureTypeIDs) == 0 {
		return false
	}
	for _, typeID := range structureTypeIDsForFilter(view) {
		if slices.Contains(destination.destination.Filters.ExcludeStructureTypeIDs, typeID) {
			return true
		}
	}
	return false
}

func structureTypeIDsForFilter(view *enrichment.Context) []string {
	if view == nil {
		return nil
	}
	if view.Event.NotificationType == "StructuresReinforcementChanged" {
		return bulkStructureTypeIDsForFilter(view)
	}
	if typeID := strings.TrimSpace(view.Event.StructureTypeID); typeID != "" {
		return []string{typeID}
	}
	for _, reference := range view.Event.StructureReferences {
		if typeID := strings.TrimSpace(reference.TypeID); typeID != "" {
			return []string{typeID}
		}
	}
	return structureTypeIDsFromStructures(view.Structures)
}

func bulkStructureTypeIDsForFilter(view *enrichment.Context) []string {
	payloadTypeIDs := make(map[string]struct{}, len(view.Event.StructureReferences))
	typeIDs := make([]string, 0, len(view.Event.StructureReferences)+len(view.Structures))
	for _, reference := range view.Event.StructureReferences {
		typeID := strings.TrimSpace(reference.TypeID)
		if typeID == "" {
			continue
		}
		typeIDs = append(typeIDs, typeID)
		if reference.ID != "" {
			payloadTypeIDs[reference.ID] = struct{}{}
		}
	}
	for index := range view.Structures {
		structure := &view.Structures[index]
		if _, ok := payloadTypeIDs[structure.ID]; ok {
			continue
		}
		if typeID := strings.TrimSpace(structure.TypeID); typeID != "" {
			typeIDs = append(typeIDs, typeID)
		}
	}
	return typeIDs
}

func structureTypeIDsFromStructures(structures []authnextdb.Structure) []string {
	typeIDs := make([]string, 0, len(structures))
	for index := range structures {
		if typeID := strings.TrimSpace(structures[index].TypeID); typeID != "" {
			typeIDs = append(typeIDs, typeID)
		}
	}
	return typeIDs
}

func webhookURLKey(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}
