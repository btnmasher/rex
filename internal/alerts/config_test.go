package alerts

import (
	"testing"

	"github.com/btnmasher/rex/internal/config"
	"github.com/btnmasher/rex/internal/notifications"
)

func TestDestinationsFromConfig(t *testing.T) {
	destinations, err := DestinationsFromConfig([]config.AlertDestination{{
		Name: "military-alerts",
		Filters: config.AlertFilters{
			AlertTypes: []string{notifications.AlertStructureCombat},
		},
		Presentation: config.Presentation{ShowEntityIDs: true},
		Delivery: config.DeliveryConfig{
			Type: "discord",
			Fields: []byte(`{
                "type":"discord",
                "targets":[{"id":"primary","webhookUrl":"https://discord.com/api/webhooks/123/token"}],
                "senderName":"Rex",
                "senderAvatarURL":"https://images.example.invalid/rex.png",
                "mentionRules":[{"alertTypes":["structures.combat"],"mention":"here"}]
            }`),
		},
	}})
	if err != nil {
		t.Fatalf("convert destinations: %v", err)
	}
	if len(destinations) != 1 || len(destinations[0].WebhookTargets) != 1 {
		t.Fatalf("unexpected destinations: %#v", destinations)
	}
	destination := destinations[0]
	if destination.WebhookTargets[0].ID != "military-alerts/primary" || !destination.Presentation.ShowEntityIDs || destination.SenderName != "Rex" {
		t.Fatalf("unexpected Discord destination: %#v", destination)
	}
	if got := mentionFor(destination.MentionRules, notifications.AlertStructureUnderAttack); got != "@here" {
		t.Fatalf("mention = %q, want @here", got)
	}
}

func TestDestinationsFromConfigRejectsProviderFields(t *testing.T) {
	_, err := DestinationsFromConfig([]config.AlertDestination{{
		Name:    "alerts",
		Filters: config.AlertFilters{AlertTypes: []string{"all"}},
		Delivery: config.DeliveryConfig{
			Type:   "discord",
			Fields: []byte(`{"type":"discord","targets":[{"id":"primary","webhookUrl":"https://discord.com/api/webhooks/123/token"}],"unknown":true}`),
		},
	}})
	if err == nil {
		t.Fatal("expected unknown provider field error")
	}
}

func TestMentionSpecificity(t *testing.T) {
	rules := []MentionRule{
		{AlertTypes: []string{"structures"}, Mention: "here"},
		{AlertTypes: []string{"structures.combat.under_attack"}, Mention: "none"},
	}
	if got := mentionFor(rules, notifications.AlertStructureUnderAttack); got != "" {
		t.Fatalf("mention = %q, want none", got)
	}
}
