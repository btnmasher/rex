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
	deliverypkg "github.com/btnmasher/rex/internal/delivery"
	"github.com/btnmasher/rex/internal/discord"
	"github.com/btnmasher/rex/internal/enrichment"
	"github.com/btnmasher/rex/internal/notifications"
	"github.com/btnmasher/rex/internal/notificationstate"
	"github.com/btnmasher/rex/internal/routing"
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

type bulkStructureAlertDatabase struct {
	structures            []authnextdb.Structure
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

func (d *bulkStructureAlertDatabase) GetStructuresByIDs(_ context.Context, structureIDs []string) ([]authnextdb.Structure, error) {
	d.requestedStructureIDs = append([]string(nil), structureIDs...)
	return d.structures, nil
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

type retryingAlertHistory struct {
	attempts   int
	failBefore int
}

func (h *alertTestHistory) RecordAlert(_ context.Context, record *notificationstate.AlertHistoryRecord) error {
	h.records = append(h.records, *record)
	return nil
}

func (h *alertTestHistory) ListAlertHistory(context.Context, time.Time, int) ([]notificationstate.AlertHistoryRecord, error) {
	return append([]notificationstate.AlertHistoryRecord(nil), h.records...), nil
}

func (h *retryingAlertHistory) RecordAlert(context.Context, *notificationstate.AlertHistoryRecord) error {
	h.attempts++
	if h.attempts <= h.failBefore {
		return errors.New("history unavailable")
	}
	return nil
}

func (h *retryingAlertHistory) ListAlertHistory(context.Context, time.Time, int) ([]notificationstate.AlertHistoryRecord, error) {
	return nil, nil
}

func TestRecordHistoryRetriesWithoutRepeatingDelivery(t *testing.T) {
	history := &retryingAlertHistory{failBefore: 2}
	record := &notificationstate.AlertHistoryRecord{NotificationID: 1}
	if err := recordHistoryWithRetry(context.Background(), history, record); err != nil {
		t.Fatalf("record history: %v", err)
	}
	if history.attempts != 3 {
		t.Fatalf("history attempts = %d, want 3", history.attempts)
	}
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
	if history.records[0].DestinationID != "a" || history.records[0].DeliveryStatus != notificationstate.AlertDeliveryStatusDelivered ||
		history.records[0].DeliveryError != "" || len(history.records[0].RawNotificationJSON) == 0 || len(history.records[0].DiscordPayloadJSON) == 0 {
		t.Fatalf("unexpected history record: %+v", history.records[0])
	}
}

func TestRecordTerminalFailureRendersEachDestinationPayload(t *testing.T) {
	history := &alertTestHistory{}
	service, err := NewService(emptyStructureAlertDatabase{}, &alertTestDelivery{}, &Config{
		Destinations: []Destination{
			{ID: "without-ids", AlertTypes: []string{notifications.AlertStructureUnderAttack}, WebhookTargets: []WebhookTarget{{ID: "without-ids/primary", URL: "https://webhook-a"}}},
			{ID: "with-ids", AlertTypes: []string{notifications.AlertStructureUnderAttack}, Presentation: Presentation{ShowEntityIDs: true}, WebhookTargets: []WebhookTarget{{ID: "with-ids/primary", URL: "https://webhook-b"}}},
		},
		History: history,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	view := &enrichment.Context{
		PollingCorporationID:   "100",
		PollingCorporationName: "Corp",
		CharacterID:            "900000001",
		Event: notifications.Event{
			NotificationID:   1,
			NotificationType: "StructureUnderAttack",
			AlertType:        notifications.AlertStructureUnderAttack,
			SystemID:         "30000142",
		},
		SystemName:          "Jita",
		RawNotificationJSON: []byte(`{"notification_id":1}`),
	}
	if err := service.RecordTerminalFailure(context.Background(), view, []string{"without-ids/primary", "with-ids/primary"}, errors.New("delivery failed")); err != nil {
		t.Fatalf("record terminal failure: %v", err)
	}
	if len(history.records) != 2 || history.records[0].DestinationID != "without-ids/primary" || history.records[1].DestinationID != "with-ids/primary" {
		t.Fatalf("unexpected terminal history records: %#v", history.records)
	}
	if bytes.Equal(history.records[0].DiscordPayloadJSON, history.records[1].DiscordPayloadJSON) {
		t.Fatal("terminal history reused one destination's Discord payload")
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

func TestDeliverTargetLogsRawPayloadWhenEnabled(t *testing.T) {
	var output bytes.Buffer
	service, err := NewService(emptyStructureAlertDatabase{}, &alertTestDelivery{}, &Config{
		Destinations: []Destination{{
			ID:          "structure-alerts",
			AlertTypes:  []string{notifications.AlertStructureUnderAttack},
			WebhookURLs: []string{"https://webhook-a"},
		}},
		LogPayloads: true,
		Logger:      slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	if outcome := service.DeliverTarget(context.Background(), &enrichment.Context{
		Event:               notifications.Event{NotificationID: 1, AlertType: notifications.AlertStructureUnderAttack},
		RawNotificationJSON: []byte(`{"type":"StructureUnderAttack"}`),
	}, "structure-alerts"); outcome.Status != deliverypkg.OutcomeAccepted {
		t.Fatalf("deliver target outcome: %+v", outcome)
	}
	if !strings.Contains(output.String(), "raw_payload=") || !strings.Contains(output.String(), "StructureUnderAttack") {
		t.Fatalf("expected raw payload in target debug log: %q", output.String())
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
				ID:              "a",
				AlertTypes:      []string{notifications.AlertStructureUnderAttack},
				WebhookURLs:     []string{"https://webhook-a"},
				SenderName:      "Rex",
				SenderAvatarURL: "https://images.example.invalid/rex.png",
			},
			{
				ID:          "b",
				AlertTypes:  []string{notifications.AlertStructureUnderAttack},
				WebhookURLs: []string{"https://webhook-b"},
			},
		},
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
		t.Fatalf("expected sender configuration, got %#v", delivery.messages[0])
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
			policy, err := routing.New([]routing.Destination{{
				ID:        "test",
				TargetIDs: []string{"target"},
				Filters: routing.Filters{
					AlertTypes:        test.configured,
					ExcludeAlertTypes: test.excluded,
				},
			}})
			if err != nil {
				t.Fatalf("new routing policy: %v", err)
			}
			got := len(policy.PreRoute("100", test.alertType)) > 0
			if got != test.want {
				t.Fatalf("routing policy selected target = %t, want %t", got, test.want)
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

func TestPostRouteDeduplicatesSharedWebhookURLs(t *testing.T) {
	service, err := NewService(emptyStructureAlertDatabase{}, &alertTestDelivery{}, &Config{
		Destinations: []Destination{
			{
				ID:         "military-alerts",
				AlertTypes: []string{notifications.AlertStructureUnderAttack},
				WebhookTargets: []WebhookTarget{{
					ID:  "military-alerts/primary",
					URL: "https://webhook-shared",
				}},
			},
			{
				ID:         "logistics-alerts",
				AlertTypes: []string{notifications.AlertStructureUnderAttack},
				WebhookTargets: []WebhookTarget{{
					ID:  "logistics-alerts/primary",
					URL: "https://webhook-shared",
				}},
			},
		},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	got := service.PreRoute(&enrichment.Envelope{
		Corporation: authnextdb.Corporation{ID: "100"},
		Event:       notifications.Event{AlertType: notifications.AlertStructureUnderAttack},
	})
	if len(got) != 2 {
		t.Fatalf("expected both targets before post-routing, got %#v", got)
	}
	postRouted := service.PostRoute(&enrichment.Context{PollingCorporationID: "100", Event: notifications.Event{
		AlertType: notifications.AlertStructureUnderAttack,
	}}, got)
	if len(postRouted) != 1 || postRouted[0] != "military-alerts/primary" {
		t.Fatalf("expected one target after shared webhook URL deduplication, got %#v", postRouted)
	}
}

func TestPostRouteRetainsSharedWebhookWhenFirstTargetIsExcluded(t *testing.T) {
	service, err := NewService(emptyStructureAlertDatabase{}, &alertTestDelivery{}, &Config{
		Destinations: []Destination{
			{
				ID:                      "excluded",
				AlertTypes:              []string{notifications.AlertStructureUnderAttack},
				ExcludeStructureTypeIDs: []string{"85230"},
				WebhookTargets:          []WebhookTarget{{ID: "excluded/primary", URL: "https://webhook-shared"}},
			},
			{
				ID:             "included",
				AlertTypes:     []string{notifications.AlertStructureUnderAttack},
				WebhookTargets: []WebhookTarget{{ID: "included/primary", URL: "https://webhook-shared"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	candidates := service.PreRoute(&enrichment.Envelope{
		Corporation: authnextdb.Corporation{ID: "100"},
		Event:       notifications.Event{AlertType: notifications.AlertStructureUnderAttack},
	})
	postRouted := service.PostRoute(&enrichment.Context{PollingCorporationID: "100", Event: notifications.Event{
		AlertType:       notifications.AlertStructureUnderAttack,
		StructureTypeID: "85230",
	}}, candidates)
	if len(postRouted) != 1 || postRouted[0] != "included/primary" {
		t.Fatalf("expected included target after post-routing, got %#v", postRouted)
	}
}

func TestDeliverFiltersDestinationsByPollingCorporation(t *testing.T) {
	delivery := &alertTestDelivery{}
	service, err := NewService(emptyStructureAlertDatabase{}, delivery, &Config{
		Destinations: []Destination{
			{
				ID:                    "included",
				AlertTypes:            []string{notifications.AlertStructureUnderAttack},
				IncludeCorporationIDs: []string{"100"},
				WebhookURLs:           []string{"https://webhook-included"},
			},
			{
				ID:                    "excluded",
				AlertTypes:            []string{notifications.AlertStructureUnderAttack},
				ExcludeCorporationIDs: []string{"100"},
				WebhookURLs:           []string{"https://webhook-excluded"},
			},
			{
				ID:          "unrestricted",
				AlertTypes:  []string{notifications.AlertStructureUnderAttack},
				WebhookURLs: []string{"https://webhook-unrestricted"},
			},
			{
				ID:                    "overridden",
				AlertTypes:            []string{notifications.AlertStructureUnderAttack},
				IncludeCorporationIDs: []string{"100"},
				ExcludeCorporationIDs: []string{"100"},
				WebhookURLs:           []string{"https://webhook-overridden"},
			},
		},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	request := func(notificationID int64, corporationID string) *DeliveryRequest {
		return &DeliveryRequest{
			Corporation: authnextdb.Corporation{ID: corporationID, Name: "Corp"},
			Event: &notifications.Event{
				NotificationID:   notificationID,
				NotificationType: "StructureUnderAttack",
				AlertType:        notifications.AlertStructureUnderAttack,
			},
		}
	}
	if err := service.Deliver(context.Background(), request(1, "100")); err != nil {
		t.Fatalf("deliver included corporation alert: %v", err)
	}
	if !sameStrings(delivery.urls, []string{"https://webhook-included", "https://webhook-unrestricted"}) {
		t.Fatalf("corporation 100 destinations = %#v", delivery.urls)
	}

	delivery.urls = nil
	if err := service.Deliver(context.Background(), request(2, "200")); err != nil {
		t.Fatalf("deliver unrestricted corporation alert: %v", err)
	}
	if !sameStrings(delivery.urls, []string{"https://webhook-excluded", "https://webhook-unrestricted"}) {
		t.Fatalf("corporation 200 destinations = %#v", delivery.urls)
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

func TestDeliverFiltersBulkStructureTypeFromPayloadBeforeDatabase(t *testing.T) {
	delivery := &alertTestDelivery{}
	service, err := NewService(emptyStructureAlertDatabase{}, delivery, &Config{
		Destinations: []Destination{
			{ID: "filtered", WebhookURLs: []string{"https://webhook-filtered"}, AlertTypes: []string{notifications.AlertStructuresReinforcementChanged}, ExcludeStructureTypeIDs: []string{"81826"}},
		},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	err = service.Deliver(context.Background(), &DeliveryRequest{
		Corporation: authnextdb.Corporation{ID: "100", Name: "Polling Corporation"},
		Event: &notifications.Event{
			NotificationID:   1,
			NotificationType: "StructuresReinforcementChanged",
			AlertType:        notifications.AlertStructuresReinforcementChanged,
			StructureReferences: []notifications.StructureReference{
				{ID: "1050629404880", TypeID: "81826"},
			},
		},
	})
	if err != nil {
		t.Fatalf("deliver bulk structure alert: %v", err)
	}
	if len(delivery.urls) != 0 {
		t.Fatalf("expected payload structure type to filter delivery: %#v", delivery.urls)
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

func TestBuildEventViewGroupsBulkReinforcementCoverage(t *testing.T) {
	firstSystem := "ZJET-E"
	secondSystem := "EL8-4Q"
	regionID := "10000015"
	region := "Pure Blind"
	firstTypeName := "Metenox Moon Drill"
	secondTypeName := "Astrahus"
	thirdTypeName := "Raitaru"
	count := 53
	weekday := 255
	database := &bulkStructureAlertDatabase{structures: []authnextdb.Structure{
		{ID: "1050629404880", TypeID: "81826", TypeName: &firstTypeName, SystemID: "30002901", SystemName: &firstSystem, RegionID: &regionID, RegionName: &region},
		{ID: "1050629404881", TypeID: "81826", TypeName: &firstTypeName, SystemID: "30002901", SystemName: &firstSystem, RegionID: &regionID, RegionName: &region},
		{ID: "1050629404882", TypeID: "35833", TypeName: &thirdTypeName, SystemID: "30002901", SystemName: &firstSystem, RegionID: &regionID, RegionName: &region},
		{ID: "1052657361104", TypeID: "35832", TypeName: &secondTypeName, SystemID: "30002902", SystemName: &secondSystem, RegionID: &regionID, RegionName: &region},
	}}
	service, err := NewService(database, &alertTestDelivery{}, &Config{})
	if err != nil {
		t.Fatalf("new alert service: %v", err)
	}
	view, err := service.buildEventView(context.Background(), &DeliveryRequest{Corporation: authnextdb.Corporation{ID: "999", Name: "Polling Corporation"}, Event: &notifications.Event{
		NotificationType: "StructuresReinforcementChanged",
		AlertType:        notifications.AlertStructuresReinforcementChanged,
		StructureIDs:     []string{"1050629404880", "1050629404881", "1050629404882", "1052657361104"},
		StructureReferences: []notifications.StructureReference{
			{ID: "1050629404880", Name: "ZJET-E - VI - 13", TypeID: "81826"},
			{ID: "1050629404881", Name: "ZJET-E - VI - 14", TypeID: "81826"},
			{ID: "1050629404882", Name: "ZJET-E - 4-1", TypeID: "35833"},
			{ID: "1052657361104", Name: "EL8-4Q - 4-1", TypeID: "35832"},
		},
		ReinforcedStructureCount: &count,
		ReinforcementWeekday:     &weekday,
	}})
	if err != nil {
		t.Fatalf("build event view: %v", err)
	}
	if len(database.requestedStructureIDs) != 4 {
		t.Fatalf("structure lookup IDs = %#v, want four IDs", database.requestedStructureIDs)
	}
	message := render(view, "Rex", "")
	embed := message.Embeds[0]
	if embed.Description != "The reinforcement schedule changed for 53 structures across 2 solar systems." {
		t.Fatalf("unexpected description: %q", embed.Description)
	}
	if !hasField(embed.Fields, "Structure Coverage", "**[Pure Blind](https://evemaps.dotlan.net/region/Pure_Blind)**\n- **[ZJET-E](https://evemaps.dotlan.net/system/ZJET-E)**\n  - **Metenox Moon Drill** (2 structures)\n    - ZJET-E - VI - 13\n    - ZJET-E - VI - 14\n  - **Raitaru** (1 structure)\n    - ZJET-E - 4-1\n- **[EL8-4Q](https://evemaps.dotlan.net/system/EL8-4Q)**\n  - **Astrahus** (1 structure)\n    - EL8-4Q - 4-1") {
		t.Fatalf("missing structure coverage summary: %#v", embed.Fields)
	}
	if !hasField(embed.Fields, "Reinforcement Schedule", "Structures: 53\nWeekday: Unchanged") {
		t.Fatalf("missing reinforcement weekday summary: %#v", embed.Fields)
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
			name:          "structure unanchoring",
			event:         notifications.Event{AlertType: notifications.AlertStructureUnanchoring, NotificationType: "StructureUnanchoring"},
			expectedColor: colorWarning,
		},
		{
			name:          "structure high power",
			event:         notifications.Event{AlertType: notifications.AlertStructureWentHighPower, NotificationType: "StructureWentHighPower"},
			expectedColor: colorSuccess,
		},
		{
			name:          "structure online",
			event:         notifications.Event{AlertType: notifications.AlertStructureOnline, NotificationType: "StructureOnline"},
			expectedColor: colorSuccess,
		},
		{
			name:          "reinforcement changed",
			event:         notifications.Event{AlertType: notifications.AlertStructuresReinforcementChanged, NotificationType: "StructuresReinforcementChanged"},
			expectedColor: colorWarning,
		},
		{
			name:          "structure vulnerable",
			event:         notifications.Event{AlertType: notifications.AlertStructureVulnerable, NotificationType: "StructureVulnerable"},
			expectedColor: colorWarning,
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
			expectedColor: colorDanger,
		},
		{
			name:          "fuel",
			event:         notifications.Event{AlertType: notifications.AlertStructureFuelAlert},
			expectedColor: colorWarning,
		},
		{
			name:          "no reagents",
			event:         notifications.Event{AlertType: notifications.AlertStructureNoReagents},
			expectedColor: colorDanger,
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
		{
			name:          "sovereignty exited reinforce",
			event:         notifications.Event{AlertType: notifications.AlertSovStationExitedReinforce, NotificationType: "SovStationExitedReinforce"},
			expectedColor: colorWarning,
		},
		{
			name:          "self-destruct canceled",
			event:         notifications.Event{AlertType: notifications.AlertSovStructureSelfDestructCancel, NotificationType: "SovStructureSelfDestructCancel"},
			expectedColor: colorWarning,
		},
		{
			name:          "moon mining extraction started",
			event:         notifications.Event{AlertType: notifications.AlertMoonminingExtractionStarted, NotificationType: "MoonminingExtractionStarted"},
			expectedColor: colorInformational,
		},
		{
			name:          "moon mining extraction canceled",
			event:         notifications.Event{AlertType: notifications.AlertMoonminingExtractionCancelled, NotificationType: "MoonminingExtractionCancelled"},
			expectedColor: colorDanger,
		},
		{
			name:          "moon mining laser fired",
			event:         notifications.Event{AlertType: notifications.AlertMoonminingLaserFired, NotificationType: "MoonminingLaserFired"},
			expectedColor: colorSuccess,
		},
		{
			name:          "moon mining automatic fracture",
			event:         notifications.Event{AlertType: notifications.AlertMoonminingAutomaticFracture, NotificationType: "MoonminingAutomaticFracture"},
			expectedColor: colorSuccess,
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

func TestAlertColorsCoverEveryAlertLeaf(t *testing.T) {
	for _, alertType := range notifications.AllAlertTypes() {
		if _, ok := alertColors[alertType]; !ok {
			t.Errorf("alert color is not defined for %q", alertType)
		}
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

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
