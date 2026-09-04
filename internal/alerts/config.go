package alerts

import (
	"fmt"
	"strings"

	"github.com/btnmasher/rex/internal/config"
	"github.com/btnmasher/rex/internal/discord"
	"github.com/btnmasher/rex/internal/notifications"
)

type discordDeliveryConfig struct {
	Type            string               `json:"type"`
	Targets         []discordTarget      `json:"targets"`
	MentionRules    []discordMentionRule `json:"mentionRules"`
	SenderName      string               `json:"senderName"`
	SenderAvatarURL string               `json:"senderAvatarURL"`
}

type discordTarget struct {
	ID         string `json:"id"`
	WebhookURL string `json:"webhookUrl"`
}

type discordMentionRule struct {
	AlertTypes []string `json:"alertTypes"`
	Mention    string   `json:"mention"`
}

// DestinationsFromConfig converts validated provider-aware configuration into
// Discord alert destinations and validates Discord-specific fields.
func DestinationsFromConfig(values []config.AlertDestination) ([]Destination, error) {
	destinations := make([]Destination, 0, len(values))
	for index := range values {
		destination, err := discordDestination(&values[index])
		if err != nil {
			return nil, err
		}
		destinations = append(destinations, destination)
	}
	return destinations, nil
}

func discordDestination(value *config.AlertDestination) (Destination, error) {
	if value == nil {
		return Destination{}, fmt.Errorf("destination is required")
	}
	if value.Delivery.Type != "discord" {
		return Destination{}, fmt.Errorf("destination %q uses unsupported delivery type %q", value.Name, value.Delivery.Type)
	}
	var deliveryConfig discordDeliveryConfig
	if err := value.Delivery.Decode(&deliveryConfig); err != nil {
		return Destination{}, fmt.Errorf("destination %q delivery: %w", value.Name, err)
	}
	if strings.ToLower(strings.TrimSpace(deliveryConfig.Type)) != "discord" {
		return Destination{}, fmt.Errorf("destination %q delivery.type must be discord", value.Name)
	}
	if len(deliveryConfig.Targets) == 0 {
		return Destination{}, fmt.Errorf("destination %q delivery.targets must not be empty", value.Name)
	}
	targets, err := discordTargets(value.Name, deliveryConfig.Targets)
	if err != nil {
		return Destination{}, err
	}
	mentionRules, err := compileMentionRules(value.Name, deliveryConfig.MentionRules)
	if err != nil {
		return Destination{}, err
	}
	return Destination{
		ID:                      value.Name,
		WebhookTargets:          targets,
		AlertTypes:              append([]string(nil), value.Filters.AlertTypes...),
		ExcludeAlertTypes:       append([]string(nil), value.Filters.ExcludeAlertTypes...),
		ExcludeStructureTypeIDs: append([]string(nil), value.Filters.ExcludeStructureTypeIDs...),
		IncludeCorporationIDs:   append([]string(nil), value.Filters.IncludeCorporationIDs...),
		ExcludeCorporationIDs:   append([]string(nil), value.Filters.ExcludeCorporationIDs...),
		Presentation:            Presentation{ShowEntityIDs: value.Presentation.ShowEntityIDs},
		MentionRules:            mentionRules,
		SenderName:              strings.TrimSpace(deliveryConfig.SenderName),
		SenderAvatarURL:         strings.TrimSpace(deliveryConfig.SenderAvatarURL),
	}, nil
}

func discordTargets(destinationName string, values []discordTarget) ([]WebhookTarget, error) {
	targets := make([]WebhookTarget, 0, len(values))
	seenIDs := make(map[string]struct{}, len(values))
	for index := range values {
		target := &values[index]
		target.ID = strings.TrimSpace(target.ID)
		target.WebhookURL = strings.TrimSpace(target.WebhookURL)
		if target.ID == "" {
			return nil, fmt.Errorf("destination %q target %d has no id", destinationName, index)
		}
		if _, exists := seenIDs[target.ID]; exists {
			return nil, fmt.Errorf("destination %q has duplicate target id %q", destinationName, target.ID)
		}
		seenIDs[target.ID] = struct{}{}
		if err := discord.ValidateWebhookURL(target.WebhookURL); err != nil {
			return nil, fmt.Errorf("destination %q target %q webhook URL: %w", destinationName, target.ID, err)
		}
		targets = append(targets, WebhookTarget{ID: destinationName + "/" + target.ID, URL: target.WebhookURL})
	}
	return targets, nil
}

func compileMentionRules(destinationName string, values []discordMentionRule) ([]MentionRule, error) {
	rules := make([]MentionRule, 0, len(values))
	seen := make(map[string]string)
	for index := range values {
		value := &values[index]
		if len(value.AlertTypes) == 0 {
			return nil, fmt.Errorf("destination %q mention rule %d has no alert types", destinationName, index)
		}
		mention := strings.ToLower(strings.TrimSpace(value.Mention))
		switch mention {
		case "none", "here", "everyone":
		default:
			return nil, fmt.Errorf("destination %q mention rule %d has invalid mention %q", destinationName, index, value.Mention)
		}
		selectors, err := normalizeMentionSelectors(destinationName, index, value.AlertTypes, mention, seen)
		if err != nil {
			return nil, err
		}
		rules = append(rules, MentionRule{AlertTypes: selectors, Mention: mention})
	}
	return rules, nil
}

func normalizeMentionSelectors(destinationName string, ruleIndex int, values []string, mention string, seen map[string]string) ([]string, error) {
	selectors := make([]string, 0, len(values))
	for _, rawSelector := range values {
		selector, err := notifications.NormalizeAlertSelector(rawSelector)
		if err != nil {
			return nil, fmt.Errorf("destination %q mention rule %d: %w", destinationName, ruleIndex, err)
		}
		specificity := strings.Count(selector, ".") + 1
		key := fmt.Sprintf("%d:%s", specificity, selector)
		if previous, exists := seen[key]; exists {
			if previous != mention {
				return nil, fmt.Errorf("destination %q has conflicting mention rules for %q", destinationName, selector)
			}
			return nil, fmt.Errorf("destination %q contains duplicate mention selector %q", destinationName, selector)
		}
		seen[key] = mention
		selectors = append(selectors, selector)
	}
	return selectors, nil
}
