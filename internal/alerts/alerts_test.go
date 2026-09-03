package alerts

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/btnmasher/rex/internal/authnextdb"
	"github.com/btnmasher/rex/internal/discord"
	"github.com/btnmasher/rex/internal/notifications"
	"github.com/btnmasher/rex/internal/notificationstate"
)

type alertTestDatabase struct {
}

type emptyStructureAlertDatabase struct {
	alertTestDatabase
}

type failingStructureAlertDatabase struct{}

type structureLookupAlertDatabase struct {
	requestedStructureIDs []string
}

func (emptyStructureAlertDatabase) GetStructuresByIDs(context.Context, []string) ([]authnextdb.Structure, error) {
	return nil, nil
}

func (failingStructureAlertDatabase) GetStructuresByIDs(context.Context, []string) ([]authnextdb.Structure, error) {
	return nil, errors.New("structure database unavailable")
}

func (d *structureLookupAlertDatabase) GetStructuresByIDs(_ context.Context, structureIDs []string) ([]authnextdb.Structure, error) {
	d.requestedStructureIDs = append([]string(nil), structureIDs...)
	return nil, nil
}

func (alertTestDatabase) GetStructuresByIDs(context.Context, []string) ([]authnextdb.Structure, error) {
	return []authnextdb.Structure{{ID: "123456789", TypeID: "35832"}}, nil
}

func (alertTestDatabase) ResolveNames(_ context.Context, kind string, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return map[string]string{}, nil
	}
	switch kind {
	case "type":
		return map[string]string{ids[0]: "Test Structure Type"}, nil
	case "alliance":
		return map[string]string{ids[0]: "Test Alliance"}, nil
	case "corporation":
		switch ids[0] {
		case "416584095":
			return map[string]string{ids[0]: "New Owner Corporation"}, nil
		case "98599770":
			return map[string]string{ids[0]: "Old Owner Corporation"}, nil
		}
	}
	return map[string]string{}, nil
}

type alertTestDelivery struct {
	failedURL string
	urls      []string
	messages  []discord.Message
}

type alertTestHistory struct {
	records []notificationstate.AlertHistoryRecord
}

func (h *alertTestHistory) RecordAlert(_ context.Context, record *notificationstate.AlertHistoryRecord) error {
	h.records = append(h.records, *record)
	return nil
}

func (h *alertTestHistory) ListAlertHistory(context.Context, time.Time, int) ([]notificationstate.AlertHistoryRecord, error) {
	return append([]notificationstate.AlertHistoryRecord(nil), h.records...), nil
}

func TestDeliverRecordsOnlyAcceptedDestinationPayloads(t *testing.T) {
	delivery := &alertTestDelivery{failedURL: "https://webhook-b"}
	history := &alertTestHistory{}
	service, err := NewService(emptyStructureAlertDatabase{}, delivery, &Config{
		Destinations: []Destination{
			{ID: "a", AlertTypes: []string{notifications.AlertStructureUnderAttack}, WebhookURLs: []string{"https://webhook-a"}},
			{ID: "b", AlertTypes: []string{notifications.AlertStructureUnderAttack}, WebhookURLs: []string{"https://webhook-b"}},
		},
		History: history,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	err = service.Deliver(context.Background(), &DeliveryRequest{
		Corporation:         authnextdb.Corporation{ID: "100", Name: "Corp"},
		CharacterID:         "900000001",
		RawNotificationJSON: []byte(`{"notification_id":1,"type":"StructureUnderAttack"}`),
		Event: &notifications.Event{
			NotificationID:   1,
			NotificationType: "StructureUnderAttack",
			AlertType:        notifications.AlertStructureUnderAttack,
		},
	})
	if err == nil || len(history.records) != 1 {
		t.Fatalf("expected one successful history record, err=%v records=%d", err, len(history.records))
	}
	if history.records[0].DestinationID != "a" || len(history.records[0].RawNotificationJSON) == 0 || len(history.records[0].DiscordPayloadJSON) == 0 {
		t.Fatalf("unexpected history record: %+v", history.records[0])
	}
}

func TestDeliverContinuesWhenStructureEnrichmentFails(t *testing.T) {
	delivery := &alertTestDelivery{}
	service, err := NewService(failingStructureAlertDatabase{}, delivery, &Config{
		Destinations: []Destination{{
			ID:          "structure-alerts",
			AlertTypes:  []string{notifications.AlertStructureUnderAttack},
			WebhookURLs: []string{"https://webhook-a"},
		}},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	err = service.Deliver(context.Background(), &DeliveryRequest{
		Corporation: authnextdb.Corporation{ID: "100", Name: "Corp"},
		CharacterID: "900000001",
		Event: &notifications.Event{
			NotificationID:   2,
			NotificationType: "StructureUnderAttack",
			AlertType:        notifications.AlertStructureUnderAttack,
			StructureIDs:     []string{"123456789"},
		},
	})
	if err != nil {
		t.Fatalf("deliver alert after structure lookup failure: %v", err)
	}
	if len(delivery.messages) != 1 {
		t.Fatalf("delivered messages = %d, want 1", len(delivery.messages))
	}
}

func TestDeliverLogsRawPayloadWhenEnabled(t *testing.T) {
	var output bytes.Buffer
	service, err := NewService(alertTestDatabase{}, &alertTestDelivery{}, &Config{
		Destinations: []Destination{
			{ID: "structure-alerts", AlertTypes: []string{notifications.AlertStructureUnderAttack}, WebhookURLs: []string{"https://webhook-a"}},
		},
		LogPayloads: true,
		Logger:      slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	rawPayload := []byte(`{"notification_id":1,"type":"StructureUnderAttack","text":"raw payload"}`)
	if err := service.Deliver(context.Background(), &DeliveryRequest{
		Corporation:         authnextdb.Corporation{ID: "100", Name: "Corp"},
		CharacterID:         "900000001",
		RawNotificationJSON: rawPayload,
		Event: &notifications.Event{
			NotificationID:   1,
			NotificationType: "StructureUnderAttack",
			AlertType:        notifications.AlertStructureUnderAttack,
		},
	}); err != nil {
		t.Fatalf("deliver alert: %v", err)
	}
	if !strings.Contains(output.String(), "raw_payload=") || !strings.Contains(output.String(), "StructureUnderAttack") || !strings.Contains(output.String(), "raw payload") {
		t.Fatalf("expected raw payload in debug log: %q", output.String())
	}
}

func (d *alertTestDelivery) Deliver(_ context.Context, destination discord.Destination, message *discord.Message) error {
	d.urls = append(d.urls, destination.WebhookURL)
	d.messages = append(d.messages, *message)
	if destination.WebhookURL == d.failedURL {
		return errors.New("destination unavailable")
	}
	return nil
}

func TestDeliverReportsOnlyFailedDestinations(t *testing.T) {
	delivery := &alertTestDelivery{failedURL: "https://webhook-b"}
	service, err := NewService(alertTestDatabase{}, delivery, &Config{
		Destinations: []Destination{
			{
				ID:          "a",
				AlertTypes:  []string{notifications.AlertStructureUnderAttack},
				WebhookURLs: []string{"https://webhook-a"},
			},
			{
				ID:          "b",
				AlertTypes:  []string{notifications.AlertStructureUnderAttack},
				WebhookURLs: []string{"https://webhook-b"},
			},
		},
		OverrideSenderName:      "Rex",
		OverrideSenderAvatarURL: "https://images.example.invalid/rex.png",
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	err = service.Deliver(context.Background(), &DeliveryRequest{
		Corporation: authnextdb.Corporation{ID: "100", Name: "Corp", Ticker: "CORP"},
		Event:       &notifications.Event{AlertType: notifications.AlertStructureUnderAttack, StructureIDs: []string{"123456789"}, Text: "alert"},
	})
	var deliveryErr *DeliveryError
	if !errors.As(err, &deliveryErr) || len(deliveryErr.DestinationIDs) != 1 || deliveryErr.DestinationIDs[0] != "b" {
		t.Fatalf("expected destination-specific error, got %v", err)
	}
	if len(delivery.urls) != 2 {
		t.Fatalf("expected both destinations to be attempted, got %v", delivery.urls)
	}
	if delivery.messages[0].Username != "Rex" || delivery.messages[0].AvatarURL != "https://images.example.invalid/rex.png" {
		t.Fatalf("expected global sender configuration, got %#v", delivery.messages[0])
	}
	if delivery.messages[0].Embeds[0].Color != colorDanger {
		t.Fatalf("unexpected canonical embed color: %#v", delivery.messages[0].Embeds[0].Color)
	}
	author := delivery.messages[0].Embeds[0].Author
	if author == nil || author.Name != "Corp" || author.IconURL != "https://images.evetech.net/corporations/100/logo?size=64" {
		t.Fatalf("unexpected corporation author: %#v", author)
	}
	if got := delivery.messages[0].Embeds[0].Thumbnail.URL; got != "https://images.evetech.net/types/35832/render?size=64" {
		t.Fatalf("unexpected structure thumbnail URL: %q", got)
	}

	delivery.urls = nil
	err = service.Deliver(context.Background(), &DeliveryRequest{
		Corporation:    authnextdb.Corporation{ID: "100", Name: "Corp", Ticker: "CORP"},
		Event:          &notifications.Event{AlertType: notifications.AlertStructureUnderAttack, StructureIDs: []string{"123456789"}, Text: "alert"},
		DestinationIDs: []string{"b"},
	})
	if err == nil || len(delivery.urls) != 1 || delivery.urls[0] != "https://webhook-b" {
		t.Fatalf("expected retry to target only failed destination, urls=%v err=%v", delivery.urls, err)
	}
}

func TestDeliverRetriesOnlyFailedWebhookTarget(t *testing.T) {
	delivery := &alertTestDelivery{failedURL: "https://webhook-b"}
	service, err := NewService(emptyStructureAlertDatabase{}, delivery, &Config{
		Destinations: []Destination{{
			ID:          "structure-alerts",
			WebhookURLs: []string{"https://webhook-a", "https://webhook-b"},
			AlertTypes:  []string{notifications.AlertStructureUnderAttack},
		}},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	err = service.Deliver(context.Background(), &DeliveryRequest{
		Corporation: authnextdb.Corporation{ID: "100", Name: "Corp"},
		Event:       &notifications.Event{AlertType: notifications.AlertStructureUnderAttack, NotificationID: 3, Text: "alert"},
	})
	var deliveryErr *DeliveryError
	if !errors.As(err, &deliveryErr) || len(deliveryErr.DestinationIDs) != 1 || deliveryErr.DestinationIDs[0] != "structure-alerts#2" {
		t.Fatalf("expected failed webhook target, got %v", err)
	}

	delivery.urls = nil
	err = service.Deliver(context.Background(), &DeliveryRequest{
		Corporation:    authnextdb.Corporation{ID: "100", Name: "Corp"},
		Event:          &notifications.Event{AlertType: notifications.AlertStructureUnderAttack, NotificationID: 3, Text: "alert"},
		DestinationIDs: []string{"structure-alerts#2"},
	})
	if err == nil || len(delivery.urls) != 1 || delivery.urls[0] != "https://webhook-b" {
		t.Fatalf("expected retry to target only failed webhook, urls=%v err=%v", delivery.urls, err)
	}
}

func TestSupportsAlertGroupsAndLeafExclusions(t *testing.T) {
	tests := []struct {
		name       string
		configured []string
		excluded   []string
		alertType  string
		want       bool
	}{
		{name: "group includes leaf", configured: []string{notifications.AlertSkyhook}, alertType: notifications.AlertSkyhookUnderAttack, want: true},
		{name: "nested group includes leaf", configured: []string{notifications.AlertStructureState}, alertType: notifications.AlertStructureUnanchoring, want: true},
		{name: "resource group includes leaf", configured: []string{notifications.AlertStructureFuel}, alertType: notifications.AlertStructureLowReagents, want: true},
		{name: "leaf includes itself", configured: []string{notifications.AlertSkyhookLostShields}, alertType: notifications.AlertSkyhookLostShields, want: true},
		{name: "exclusion wins", configured: []string{notifications.AlertSkyhook}, excluded: []string{notifications.AlertSkyhookLostShields}, alertType: notifications.AlertSkyhookLostShields, want: false},
		{name: "all includes leaf", configured: []string{"all"}, alertType: notifications.AlertSkyhookDestroyed, want: true},
		{name: "unrelated leaf omitted", configured: []string{notifications.AlertSkyhook}, alertType: notifications.AlertStructureReinforced, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := supports(test.configured, test.excluded, test.alertType); got != test.want {
				t.Fatalf("supports() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestDeliverDeduplicatesParentAndLeafMatches(t *testing.T) {
	delivery := &alertTestDelivery{}
	service, err := NewService(emptyStructureAlertDatabase{}, delivery, &Config{
		Destinations: []Destination{{
			ID:          "skyhook-alerts",
			AlertTypes:  []string{notifications.AlertSkyhook, notifications.AlertSkyhookUnderAttack},
			WebhookURLs: []string{"https://webhook-skyhook"},
		}},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	err = service.Deliver(context.Background(), &DeliveryRequest{
		Corporation: authnextdb.Corporation{ID: "100", Name: "Polling Corporation"},
		Event: &notifications.Event{
			NotificationID:   1,
			NotificationType: "SkyhookUnderAttack",
			AlertType:        notifications.AlertSkyhookUnderAttack,
		},
	})
	if err != nil {
		t.Fatalf("deliver alert: %v", err)
	}
	if len(delivery.urls) != 1 {
		t.Fatalf("expected one delivery for parent and leaf match, got %d", len(delivery.urls))
	}
}

func TestDeliverDeduplicatesSharedWebhookURLsAcrossDestinations(t *testing.T) {
	delivery := &alertTestDelivery{}
	service, err := NewService(emptyStructureAlertDatabase{}, delivery, &Config{
		Destinations: []Destination{
			{ID: "military-alerts", AlertTypes: []string{notifications.AlertStructureUnderAttack}, WebhookURLs: []string{"https://webhook-shared"}},
			{ID: "logistics-alerts", AlertTypes: []string{notifications.AlertStructureUnderAttack}, WebhookURLs: []string{"https://webhook-shared"}},
		},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	err = service.Deliver(context.Background(), &DeliveryRequest{
		Corporation: authnextdb.Corporation{ID: "100", Name: "Polling Corporation"},
		Event: &notifications.Event{
			NotificationID:   1,
			NotificationType: "StructureUnderAttack",
			AlertType:        notifications.AlertStructureUnderAttack,
		},
	})
	if err != nil {
		t.Fatalf("deliver alert: %v", err)
	}
	if len(delivery.urls) != 1 || delivery.urls[0] != "https://webhook-shared" {
		t.Fatalf("expected one delivery for shared webhook URL, got %#v", delivery.urls)
	}
}

func TestDeliverFiltersDestinationByPayloadStructureType(t *testing.T) {
	delivery := &alertTestDelivery{}
	service, err := NewService(alertTestDatabase{}, delivery, &Config{
		Destinations: []Destination{
			{ID: "filtered", WebhookURLs: []string{"https://webhook-filtered"}, AlertTypes: []string{notifications.AlertStructureUnderAttack}, ExcludeStructureTypeIDs: []string{"81080"}},
			{ID: "allowed", WebhookURLs: []string{"https://webhook-allowed"}, AlertTypes: []string{notifications.AlertStructureUnderAttack}},
		},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	err = service.Deliver(context.Background(), &DeliveryRequest{
		Corporation: authnextdb.Corporation{ID: "100", Name: "Polling Corporation"},
		Event: &notifications.Event{
			NotificationID:   1,
			NotificationType: "StructureUnderAttack",
			AlertType:        notifications.AlertStructureUnderAttack,
			StructureIDs:     []string{"123456789"},
			StructureTypeID:  "81080",
		},
	})
	if err != nil {
		t.Fatalf("deliver structure alert: %v", err)
	}
	if len(delivery.urls) != 1 || delivery.urls[0] != "https://webhook-allowed" {
		t.Fatalf("unexpected destination deliveries: %#v", delivery.urls)
	}
}

func TestDeliverFiltersDestinationByEnrichedStructureType(t *testing.T) {
	delivery := &alertTestDelivery{}
	service, err := NewService(alertTestDatabase{}, delivery, &Config{
		Destinations: []Destination{{
			ID:                      "filtered",
			AlertTypes:              []string{notifications.AlertStructureUnderAttack},
			ExcludeStructureTypeIDs: []string{"35832"},
			WebhookURLs:             []string{"https://webhook-filtered"},
		}},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	err = service.Deliver(context.Background(), &DeliveryRequest{
		Corporation: authnextdb.Corporation{ID: "100", Name: "Polling Corporation"},
		Event: &notifications.Event{
			NotificationID:   1,
			NotificationType: "StructureUnderAttack",
			AlertType:        notifications.AlertStructureUnderAttack,
			StructureIDs:     []string{"123456789"},
		},
	})
	if err != nil {
		t.Fatalf("deliver structure alert: %v", err)
	}
	if len(delivery.urls) != 0 {
		t.Fatalf("expected enriched structure type to filter delivery: %#v", delivery.urls)
	}
}

func TestDeliverUsesAllianceAuthorForSovereigntyClaim(t *testing.T) {
	delivery := &alertTestDelivery{}
	service, err := NewService(alertTestDatabase{}, delivery, &Config{
		Destinations: []Destination{{
			ID:          "alliance-alerts",
			AlertTypes:  []string{notifications.AlertSovereignty},
			WebhookURLs: []string{"https://webhook-alliance"},
		}},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	timestamp := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	err = service.Deliver(context.Background(), &DeliveryRequest{
		Corporation: authnextdb.Corporation{ID: "100", Name: "Corp"},
		Event: &notifications.Event{
			NotificationID:   1,
			NotificationType: "SovAllClaimAquiredMsg",
			AlertType:        notifications.AlertSovAllClaimAcquiredMsg,
			AllianceID:       "99000001",
			Timestamp:        timestamp,
		},
	})
	if err != nil {
		t.Fatalf("deliver alliance alert: %v", err)
	}
	if len(delivery.messages) != 1 {
		t.Fatalf("expected one message, got %d", len(delivery.messages))
	}
	embed := delivery.messages[0].Embeds[0]
	if embed.Author == nil || embed.Author.Name != "Test Alliance" || embed.Author.IconURL != "https://images.evetech.net/alliances/99000001/logo?size=64" {
		t.Fatalf("unexpected alliance author: %#v", embed.Author)
	}
	if !hasField(embed.Fields, "Alliance", "[Test Alliance](https://evewho.com/alliance/99000001)") {
		t.Fatalf("unexpected alliance field: %#v", embed.Fields)
	}
}

func TestDeliverOmitsCorporationForNonStructureAlert(t *testing.T) {
	delivery := &alertTestDelivery{}
	service, err := NewService(alertTestDatabase{}, delivery, &Config{
		Destinations: []Destination{{
			ID:          "ess-alerts",
			AlertTypes:  []string{notifications.AlertESSMainBankLink},
			WebhookURLs: []string{"https://webhook-ess"},
		}},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	err = service.Deliver(context.Background(), &DeliveryRequest{
		Corporation: authnextdb.Corporation{ID: "100", Name: "Corp"},
		Event: &notifications.Event{
			NotificationID:   1,
			NotificationType: "ESSMainBankLink",
			AlertType:        notifications.AlertESSMainBankLink,
		},
	})
	if err != nil {
		t.Fatalf("deliver ESS alert: %v", err)
	}
	embed := delivery.messages[0].Embeds[0]
	if embed.Author != nil {
		t.Fatalf("expected no corporation author for non-structure alert: %#v", embed.Author)
	}
	for _, field := range embed.Fields {
		if field.Name == "Corporation" {
			t.Fatalf("expected no corporation field for non-structure alert: %#v", embed.Fields)
		}
	}
}

func TestDeliverUsesPayloadStructureOwner(t *testing.T) {
	delivery := &alertTestDelivery{}
	service, err := NewService(emptyStructureAlertDatabase{}, delivery, &Config{
		Destinations: []Destination{{
			ID:          "structure-alerts",
			AlertTypes:  []string{notifications.AlertStructureUnderAttack},
			WebhookURLs: []string{"https://webhook-structure"},
		}},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	err = service.Deliver(context.Background(), &DeliveryRequest{
		Corporation: authnextdb.Corporation{ID: "100", Name: "Polling Corp", Ticker: "POLL"},
		Event: &notifications.Event{
			NotificationID:       1,
			NotificationType:     "StructureUnderAttack",
			AlertType:            notifications.AlertStructureUnderAttack,
			StructureIDs:         []string{"123456789"},
			OwnerCorporationID:   "200",
			OwnerCorporationName: "Different Corp",
		},
	})
	if err != nil {
		t.Fatalf("deliver structure alert: %v", err)
	}
	embed := delivery.messages[0].Embeds[0]
	if embed.Author == nil || embed.Author.Name != "Different Corp" || embed.Author.IconURL != "https://images.evetech.net/corporations/200/logo?size=64" {
		t.Fatalf("expected payload corporation author, got %#v", embed.Author)
	}
	if !hasField(embed.Fields, "Corporation", "[Different Corp](https://evewho.com/corporation/200)") {
		t.Fatalf("expected payload corporation field, got %#v", embed.Fields)
	}
}

func TestDeliverUsesStructureIDsForStructureEnrichment(t *testing.T) {
	database := &structureLookupAlertDatabase{}
	delivery := &alertTestDelivery{}
	service, err := NewService(database, delivery, &Config{Destinations: []Destination{{
		ID:          "structure-alerts",
		AlertTypes:  []string{notifications.AlertStructureUnderAttack},
		WebhookURLs: []string{"https://webhook-structure"},
	}}})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	err = service.Deliver(context.Background(), &DeliveryRequest{
		Corporation: authnextdb.Corporation{ID: "100", Name: "Polling Corp"},
		Event: &notifications.Event{
			NotificationID:       1,
			NotificationType:     "StructureUnderAttack",
			AlertType:            notifications.AlertStructureUnderAttack,
			StructureIDs:         []string{"123456789"},
			OwnerCorporationID:   "200",
			OwnerCorporationName: "Structure Owner",
		},
	})
	if err != nil {
		t.Fatalf("deliver structure alert: %v", err)
	}
	if len(database.requestedStructureIDs) != 1 || database.requestedStructureIDs[0] != "123456789" {
		t.Fatalf("structure lookup IDs = %#v, want [123456789]", database.requestedStructureIDs)
	}
}

func TestDeliverUsesPollingCorporationWhenStructureIsNotEnriched(t *testing.T) {
	delivery := &alertTestDelivery{}
	service, err := NewService(emptyStructureAlertDatabase{}, delivery, &Config{
		Destinations: []Destination{{
			ID:          "structure-alerts",
			AlertTypes:  []string{notifications.AlertStructureState},
			WebhookURLs: []string{"https://webhook-structure"},
		}},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	err = service.Deliver(context.Background(), &DeliveryRequest{
		Corporation: authnextdb.Corporation{ID: "100", Name: "Polling Corporation", Ticker: "POLL"},
		Event: &notifications.Event{
			NotificationID:   1,
			NotificationType: "StructureAnchoring",
			AlertType:        notifications.AlertStructureAnchoring,
			StructureIDs:     []string{"123456789"},
			StructureTypeID:  "81826",
			AllianceID:       "99000001",
		},
	})
	if err != nil {
		t.Fatalf("deliver structure alert: %v", err)
	}
	embed := delivery.messages[0].Embeds[0]
	if embed.Author == nil || embed.Author.Name != "Polling Corporation" || embed.Author.IconURL != "https://images.evetech.net/corporations/100/logo?size=64" {
		t.Fatalf("expected polling corporation author, got %#v", embed.Author)
	}
	if !hasField(embed.Fields, "Corporation", "[Polling Corporation](https://evewho.com/corporation/100) [POLL]") {
		t.Fatalf("expected polling corporation field, got %#v", embed.Fields)
	}
	if !hasField(embed.Fields, "Structure Type", "Test Structure Type") {
		t.Fatalf("expected structure type field, got %#v", embed.Fields)
	}
}

func TestAlertColorDefaults(t *testing.T) {
	tests := []struct {
		name          string
		event         notifications.Event
		expectedColor int
	}{
		{
			name:          "structure attack",
			event:         notifications.Event{AlertType: notifications.AlertStructureUnderAttack, NotificationType: "StructureUnderAttack"},
			expectedColor: colorDanger,
		},
		{
			name:          "starbase attack",
			event:         notifications.Event{AlertType: notifications.AlertStarbaseUnderAttack, NotificationType: "TowerAlertMsg"},
			expectedColor: colorDanger,
		},
		{
			name:          "starbase resource alert",
			event:         notifications.Event{AlertType: notifications.AlertStarbaseResourceAlert, NotificationType: "TowerResourceAlertMsg"},
			expectedColor: colorWarning,
		},
		{
			name:          "customs office attack",
			event:         notifications.Event{AlertType: notifications.AlertCustomsOfficeAttacked, NotificationType: "OrbitalAttacked"},
			expectedColor: colorDanger,
		},
		{
			name:          "structure anchoring",
			event:         notifications.Event{AlertType: notifications.AlertStructureAnchoring, NotificationType: "StructureAnchoring"},
			expectedColor: colorInformational,
		},
		{
			name:          "ownership transfer",
			event:         notifications.Event{AlertType: notifications.AlertStructureOwnership, NotificationType: "OwnershipTransferred"},
			expectedColor: colorInformational,
		},
		{
			name:          "skyhook attack",
			event:         notifications.Event{AlertType: notifications.AlertSkyhookUnderAttack, NotificationType: "SkyhookUnderAttack"},
			expectedColor: colorDanger,
		},
		{
			name:          "ess",
			event:         notifications.Event{AlertType: notifications.AlertESSMainBankLink},
			expectedColor: colorWarning,
		},
		{
			name:          "fuel",
			event:         notifications.Event{AlertType: notifications.AlertStructureFuelAlert},
			expectedColor: colorWarning,
		},
		{
			name:          "services offline",
			event:         notifications.Event{AlertType: notifications.AlertStructureOffline, NotificationType: "StructureServicesOffline"},
			expectedColor: colorWarning,
		},
		{
			name:          "entosis",
			event:         notifications.Event{AlertType: notifications.AlertEntosisCaptureStarted},
			expectedColor: colorDanger,
		},
		{
			name:          "sovereignty claimed",
			event:         notifications.Event{AlertType: notifications.AlertSovAllClaimAcquiredMsg, NotificationType: "SovAllClaimAquiredMsg"},
			expectedColor: colorSuccess,
		},
		{
			name:          "sovereignty lost",
			event:         notifications.Event{AlertType: notifications.AlertSovAllClaimLostMsg, NotificationType: "SovAllClaimLostMsg"},
			expectedColor: colorDanger,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := alertColor(&test.event); got != test.expectedColor {
				t.Fatalf("alertColor() = %#x, want %#x", got, test.expectedColor)
			}
		})
	}
}

func TestRenderSkyhookUsesPayloadMetadata(t *testing.T) {
	shield := 94.6
	armor := 100.0
	hull := 100.0
	timestamp := time.Date(2026, time.September, 2, 2, 5, 0, 0, time.UTC)
	message := render(&eventView{
		AllianceName: "The Caldari Fourth District",
		Event: notifications.Event{
			NotificationType:        "SkyhookUnderAttack",
			AlertType:               notifications.AlertSkyhookUnderAttack,
			AllianceID:              "99013797",
			StructureIDs:            []string{"1046813349750"},
			StructureTypeID:         "81080",
			SystemID:                "30001988",
			Timestamp:               timestamp,
			ShieldPercentage:        &shield,
			ArmorPercentage:         &armor,
			HullPercentage:          &hull,
			AttackerCharacterID:     "2119766668",
			AttackerCharacterName:   "Attacking Pilot",
			AttackerCorporationID:   "98774989",
			AttackerCorporationName: "Attacking Corporation",
		},
	}, "Rex Alerts", "")
	embed := message.Embeds[0]
	if embed.Title != "Skyhook Under Attack" {
		t.Fatalf("unexpected title: %q", embed.Title)
	}
	if strings.Contains(embed.Description, "shields") || strings.Contains(embed.Description, "armor") || strings.Contains(embed.Description, "hull") {
		t.Fatalf("expected integrity to be omitted from description: %q", embed.Description)
	}
	if embed.Thumbnail == nil || embed.Thumbnail.URL != "https://images.evetech.net/types/81080/render?size=64" {
		t.Fatalf("unexpected structure thumbnail: %#v", embed.Thumbnail)
	}
	if embed.Author == nil || embed.Author.Name != "The Caldari Fourth District" {
		t.Fatalf("unexpected alliance author: %#v", embed.Author)
	}
	if !hasField(embed.Fields, "Attacker", "[Attacking Pilot](https://evewho.com/character/2119766668)\n[Attacking Corporation](https://evewho.com/corporation/98774989)") {
		t.Fatalf("expected attacker field: %#v", embed.Fields)
	}
	if !hasField(embed.Fields, "Integrity", "Shields: **94.6%**\nArmor: **100.0%**\nHull: **100.0%**") {
		t.Fatalf("expected integrity field: %#v", embed.Fields)
	}
}

func TestRenderSkyhookLostShieldsUsesReinforcedState(t *testing.T) {
	timestamp := time.Date(2026, time.September, 2, 2, 11, 0, 0, time.UTC)
	message := render(&eventView{
		SystemName: "MI6O-6",
		Event: notifications.Event{
			NotificationType: "SkyhookLostShields",
			AlertType:        notifications.AlertSkyhookLostShields,
			StructureIDs:     []string{"1046813349750"},
			StructureTypeID:  "81080",
			SystemID:         "30001988",
			Timestamp:        timestamp,
			TimeLeft:         45 * time.Hour,
			VulnerableFor:    15 * time.Minute,
		},
	}, "Rex Alerts", "")

	embed := message.Embeds[0]
	if embed.Description != "A skyhook has lost its shields and entered reinforced mode in [MI6O-6](https://evemaps.dotlan.net/system/MI6O-6)." {
		t.Fatalf("unexpected description: %q", embed.Description)
	}
	if !hasField(embed.Fields, "Reinforced Until", timeRemainingValue(timestamp, 45*time.Hour)) {
		t.Fatalf("reinforcement timer missing: %#v", embed.Fields)
	}
	for _, field := range embed.Fields {
		if field.Name == "Vulnerable Until" {
			t.Fatalf("unexpected vulnerability timer: %#v", embed.Fields)
		}
	}
}

func TestRenderStarbaseAlerts(t *testing.T) {
	shield := 50.0
	armor := 75.0
	hull := 100.0
	attackMessage := render(&eventView{
		CorporationOwned: true,
		CorporationID:    "98790350",
		CorporationName:  "Starbase Owner",
		SystemName:       "Jita",
		Event: notifications.Event{
			NotificationType:        "TowerAlertMsg",
			AlertType:               notifications.AlertStarbaseUnderAttack,
			SystemID:                "30000142",
			Timestamp:               time.Date(2026, time.September, 2, 2, 5, 0, 0, time.UTC),
			StructureTypeID:         "35834",
			AttackerAllianceID:      "498125261",
			AttackerAllianceName:    "Attacking Alliance",
			AttackerCharacterID:     "2119766668",
			AttackerCharacterName:   "Attacking Pilot",
			AttackerCorporationID:   "98774989",
			AttackerCorporationName: "Attacking Corporation",
			ShieldValue:             &shield,
			ArmorValue:              &armor,
			HullValue:               &hull,
		},
	}, "Rex Alerts", "")
	attackEmbed := attackMessage.Embeds[0]
	if attackEmbed.Title != "Starbase Under Attack" || attackEmbed.Description != "A starbase is under attack in [Jita](https://evemaps.dotlan.net/system/Jita)." {
		t.Fatalf("unexpected starbase attack embed: %#v", attackEmbed)
	}
	if !hasField(attackEmbed.Fields, "Integrity", "Shield value: **50**\nArmor value: **75**\nHull value: **100**") {
		t.Fatalf("expected starbase integrity field: %#v", attackEmbed.Fields)
	}
	if !hasField(attackEmbed.Fields, "Attacker", "[Attacking Pilot](https://evewho.com/character/2119766668)\n[Attacking Corporation](https://evewho.com/corporation/98774989)\nAlliance: [Attacking Alliance](https://evewho.com/alliance/498125261)") {
		t.Fatalf("expected starbase attacker field: %#v", attackEmbed.Fields)
	}

	resourceMessage := render(&eventView{
		CorporationOwned: true,
		CorporationID:    "98790350",
		CorporationName:  "Starbase Owner",
		SystemName:       "Jita",
		Event: notifications.Event{
			NotificationType: "TowerResourceAlertMsg",
			AlertType:        notifications.AlertStarbaseResourceAlert,
			SystemID:         "30000142",
			Timestamp:        time.Date(2026, time.September, 2, 2, 5, 0, 0, time.UTC),
			ResourceRequirements: []notifications.ResourceRequirement{
				{Quantity: 100, TypeID: "4247"},
			},
		},
	}, "Rex Alerts", "")
	if !hasField(resourceMessage.Embeds[0].Fields, "Resources Needed", "100 x Type 4247") {
		t.Fatalf("expected starbase resource field: %#v", resourceMessage.Embeds[0].Fields)
	}
}

func TestRenderCustomsOfficeAlerts(t *testing.T) {
	shield := 94.5
	attackMessage := render(&eventView{
		CorporationOwned: true,
		CorporationID:    "98790350",
		CorporationName:  "Customs Office Owner",
		SystemName:       "Jita",
		PlanetName:       "Jita IV",
		Event: notifications.Event{
			NotificationType:      "OrbitalAttacked",
			AlertType:             notifications.AlertCustomsOfficeAttacked,
			SystemID:              "30000142",
			PlanetID:              "40126936",
			Timestamp:             time.Date(2026, time.September, 2, 2, 5, 0, 0, time.UTC),
			StructureTypeID:       "2233",
			AttackerAllianceID:    "498125261",
			AttackerAllianceName:  "Attacking Alliance",
			AttackerCharacterID:   "2119766668",
			AttackerCharacterName: "Attacking Pilot",
			AttackerCorporationID: "98774989",
			ShieldPercentage:      &shield,
		},
	}, "Rex Alerts", "")
	attackEmbed := attackMessage.Embeds[0]
	if attackEmbed.Title != "Customs Office Under Attack" || attackEmbed.Description != "A customs office is under attack in [Jita](https://evemaps.dotlan.net/system/Jita)." {
		t.Fatalf("unexpected customs office attack embed: %#v", attackEmbed)
	}
	if !hasField(attackEmbed.Fields, "Planet", "Jita IV") || !hasField(attackEmbed.Fields, "Integrity", "Shields: **94.5%**") {
		t.Fatalf("expected customs office location and integrity fields: %#v", attackEmbed.Fields)
	}
	if !hasFieldNamed(attackEmbed.Fields, "Attacker") {
		t.Fatalf("expected customs office attacker field: %#v", attackEmbed.Fields)
	}

	reinforcedUntil := time.Date(2026, time.September, 2, 3, 5, 0, 0, time.UTC)
	reinforcedMessage := render(&eventView{
		SystemName: "Jita",
		Event: notifications.Event{
			NotificationType: "OrbitalReinforced",
			AlertType:        notifications.AlertCustomsOfficeReinforced,
			SystemID:         "30000142",
			Timestamp:        time.Date(2026, time.September, 2, 2, 5, 0, 0, time.UTC),
			ReinforcedUntil:  reinforcedUntil,
		},
	}, "Rex Alerts", "")
	if !hasField(reinforcedMessage.Embeds[0].Fields, "Reinforced Until", timestampValue(reinforcedUntil)) {
		t.Fatalf("expected customs office reinforcement timer: %#v", reinforcedMessage.Embeds[0].Fields)
	}
}

func TestRenderExpandedNotificationFields(t *testing.T) {
	timestamp := time.Date(2026, time.September, 2, 2, 5, 0, 0, time.UTC)
	message := render(&eventView{
		SystemName: "Jita",
		Event: notifications.Event{
			NotificationType:   "MoonminingExtractionStarted",
			AlertType:          notifications.AlertMoonminingExtractionStarted,
			SystemID:           "30000142",
			Timestamp:          timestamp,
			ActorCharacterID:   "2119766668",
			ActorCharacterName: "Mining Director",
			StructureIDs:       []string{"1043576893913"},
			StructureTypeID:    "35834",
			StructureName:      "Moon Drill",
			ReadyAt:            timestamp.Add(time.Hour),
			AutoAt:             timestamp.Add(2 * time.Hour),
		},
	}, "Rex", "")
	embed := message.Embeds[0]
	if embed.Description != "A moon mining extraction has started in [Jita](https://evemaps.dotlan.net/system/Jita)." {
		t.Fatalf("unexpected description: %q", embed.Description)
	}
	if !hasField(embed.Fields, "Actor", "[Mining Director](https://evewho.com/character/2119766668)") {
		t.Fatalf("expected actor field: %#v", embed.Fields)
	}
	if !hasField(embed.Fields, "Ready At", timestampValue(timestamp.Add(time.Hour))) || !hasField(embed.Fields, "Automatic Fracture At", timestampValue(timestamp.Add(2*time.Hour))) {
		t.Fatalf("expected moon mining activity fields: %#v", embed.Fields)
	}
}

func TestDeliverIncludesOwnershipTransferDetails(t *testing.T) {
	delivery := &alertTestDelivery{}
	service, err := NewService(alertTestDatabase{}, delivery, &Config{
		Destinations: []Destination{{
			ID:          "structure-alerts",
			AlertTypes:  []string{notifications.AlertStructureOwnership},
			WebhookURLs: []string{"https://webhook-structure"},
		}},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	err = service.Deliver(context.Background(), &DeliveryRequest{
		Corporation: authnextdb.Corporation{ID: "100", Name: "Polling Corporation"},
		Event: &notifications.Event{
			NotificationType:      "OwnershipTransferred",
			AlertType:             notifications.AlertStructureOwnership,
			StructureIDs:          []string{"123456789"},
			NewOwnerCorporationID: "416584095",
			OldOwnerCorporationID: "98599770",
		},
	})
	if err != nil {
		t.Fatalf("deliver ownership alert: %v", err)
	}
	if len(delivery.messages) != 1 {
		t.Fatalf("expected one message, got %d", len(delivery.messages))
	}
	if !hasField(delivery.messages[0].Embeds[0].Fields, "Ownership", "Previous owner: [Old Owner Corporation](https://evewho.com/corporation/98599770)\nNew owner: [New Owner Corporation](https://evewho.com/corporation/416584095)") {
		t.Fatalf("expected ownership transfer details: %#v", delivery.messages[0].Embeds[0].Fields)
	}
	if !hasFieldNamed(delivery.messages[0].Embeds[0].Fields, "Structure") || hasFieldNamed(delivery.messages[0].Embeds[0].Fields, "Structures") {
		t.Fatalf("expected singular structure field: %#v", delivery.messages[0].Embeds[0].Fields)
	}
}

func TestRenderLinksIdentitiesAndLocations(t *testing.T) {
	message := render(&eventView{
		CorporationID:     "416584095",
		CorporationName:   "Fourth District Sentinels",
		CorporationTicker: "4DS",
		CorporationOwned:  true,
		SystemName:        "RD-G2R",
		RegionID:          "10000015",
		RegionName:        "Pure Blind",
		AllianceName:      "The Caldari Fourth District",
		Event: notifications.Event{
			NotificationType:        "StructureUnderAttack",
			AlertType:               notifications.AlertStructureUnderAttack,
			StructureIDs:            []string{"1046813349750"},
			SystemID:                "30001988",
			AllianceID:              "498125261",
			AttackerCharacterID:     "2118513540",
			AttackerCharacterName:   "Attacking Pilot",
			AttackerCorporationID:   "416584095",
			AttackerCorporationName: "Attacking Corporation",
			Timestamp:               time.Date(2026, time.September, 2, 2, 5, 0, 0, time.UTC),
		},
	}, "Rex Alerts", "")

	fields := message.Embeds[0].Fields
	if !hasField(fields, "Corporation", "[Fourth District Sentinels](https://evewho.com/corporation/416584095) [4DS]") {
		t.Fatalf("unexpected corporation field: %#v", fields)
	}
	if !hasField(fields, "Attacker", "[Attacking Pilot](https://evewho.com/character/2118513540)\n[Attacking Corporation](https://evewho.com/corporation/416584095)") {
		t.Fatalf("unexpected attacker field: %#v", fields)
	}
	if !hasField(fields, "Alliance", "[The Caldari Fourth District](https://evewho.com/alliance/498125261)") {
		t.Fatalf("unexpected alliance field: %#v", fields)
	}
	if !hasField(fields, "Solar System", "[RD-G2R](https://evemaps.dotlan.net/system/RD-G2R)") {
		t.Fatalf("unexpected solar system field: %#v", fields)
	}
	if !hasField(fields, "Region", "[Pure Blind](https://evemaps.dotlan.net/region/Pure_Blind)") {
		t.Fatalf("unexpected region field: %#v", fields)
	}
}

func TestRenderLinksCanIncludeEntityIDs(t *testing.T) {
	message := render(&eventView{
		CorporationID:     "416584095",
		CorporationName:   "Fourth District Sentinels",
		CorporationTicker: "4DS",
		CorporationOwned:  true,
		SystemName:        "RD-G2R",
		RegionID:          "10000015",
		RegionName:        "Pure Blind",
		AllianceName:      "The Caldari Fourth District",
		ShowEntityIDs:     true,
		Event: notifications.Event{
			NotificationType:      "StructureUnderAttack",
			AlertType:             notifications.AlertStructureUnderAttack,
			SystemID:              "30001988",
			AllianceID:            "498125261",
			AllianceName:          "The Caldari Fourth District",
			AttackerCharacterID:   "2118513540",
			AttackerCharacterName: "Attacking Pilot",
			Timestamp:             time.Date(2026, time.September, 2, 2, 5, 0, 0, time.UTC),
		},
	}, "Rex Alerts", "")

	fields := message.Embeds[0].Fields
	if !hasField(fields, "Corporation", "[Fourth District Sentinels](https://evewho.com/corporation/416584095) (416584095) [4DS]") {
		t.Fatalf("unexpected corporation field: %#v", fields)
	}
	if !hasField(fields, "Alliance", "[The Caldari Fourth District](https://evewho.com/alliance/498125261) (498125261)") {
		t.Fatalf("unexpected alliance field: %#v", fields)
	}
	if !hasField(fields, "Solar System", "[RD-G2R](https://evemaps.dotlan.net/system/RD-G2R) (30001988)") {
		t.Fatalf("unexpected solar system field: %#v", fields)
	}
	if !hasField(fields, "Region", "[Pure Blind](https://evemaps.dotlan.net/region/Pure_Blind) (10000015)") {
		t.Fatalf("unexpected region field: %#v", fields)
	}
}

func TestEventDescriptionPrefersCanonicalTemplateAndEscapesFallback(t *testing.T) {
	if got := eventDescription(&eventView{Event: notifications.Event{
		NotificationType: "SovereigntyClaimed",
		Summary:          "Your alliance has claimed sovereignty.",
	}}); got != "Sovereignty has been claimed in the reported system." {
		t.Fatalf("canonical description = %q", got)
	}
	if got := eventDescription(&eventView{Event: notifications.Event{
		NotificationType: "UnknownNotification",
		Summary:          "A *bold* _name_ [link] `code` ~spoiler~ > quote | value",
	}}); got != "A \\*bold\\* \\_name\\_ \\[link\\] \\`code\\` \\~spoiler\\~ \\> quote \\| value" {
		t.Fatalf("escaped fallback description = %q", got)
	}
}

func TestRenderRetainsAllSupportedAlertFields(t *testing.T) {
	timestamp := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	shield := 94.6
	message := render(&eventView{
		CorporationID:    "100",
		CorporationName:  "Corp",
		CorporationOwned: true,
		SystemName:       "Jita",
		RegionID:         "10000002",
		RegionName:       "The Forge",
		AllianceName:     "Alliance",
		Event: notifications.Event{
			NotificationType: "StructureAnchoring",
			AlertType:        notifications.AlertStructureAnchoring,
			NotificationID:   1,
			StructureIDs:     []string{"1043576893913"},
			StructureTypeID:  "35834",
			SystemID:         "30000142",
			AllianceID:       "99000001",
			Timestamp:        timestamp,
			TimeLeft:         10 * time.Minute,
			VulnerableFor:    5 * time.Minute,
			DecloakAt:        timestamp.Add(time.Hour),
			ShieldPercentage: &shield,
		},
	}, "Rex", "")
	fields := message.Embeds[0].Fields
	if len(fields) != 11 {
		t.Fatalf("field count = %d, want 11: %#v", len(fields), fields)
	}
	if hasFieldNamed(fields, "Notification") {
		t.Fatalf("redundant notification field present: %#v", fields)
	}
	if !hasField(fields, "Decloak At", timestampValue(timestamp.Add(time.Hour))) {
		t.Fatalf("decloak field missing: %#v", fields)
	}
}

func TestBuildFieldsHandlesNilView(t *testing.T) {
	if fields := buildFields(nil); fields != nil {
		t.Fatalf("fields for nil view = %#v, want nil", fields)
	}
}

func hasField(fields []discord.Field, name, value string) bool {
	for _, field := range fields {
		if field.Name == name && field.Value == value {
			return true
		}
	}
	return false
}

func hasFieldNamed(fields []discord.Field, name string) bool {
	for _, field := range fields {
		if field.Name == name {
			return true
		}
	}

	return false
}
