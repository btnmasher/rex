// Package alerts resolves destinations and renders classified notification alerts.
package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/btnmasher/rex/internal/authnextdb"
	"github.com/btnmasher/rex/internal/delivery"
	"github.com/btnmasher/rex/internal/discord"
	"github.com/btnmasher/rex/internal/enrichment"
	"github.com/btnmasher/rex/internal/notifications"
	"github.com/btnmasher/rex/internal/notificationstate"
	"github.com/btnmasher/rex/internal/routing"
)

const (
	colorDanger                   = 0xD7263D
	colorWarning                  = 0xF1C40F
	colorSuccess                  = 0x2ECC71
	colorInformational            = 0x2D6CDF
	maxTextSize                   = 3800
	maxStructureSummarySize       = 1024
	maxIntegrityValues            = 3
	maxAttackerParts              = 3
	maxOwnershipParts             = 2
	maxTimerFields                = 4
	maxActivityFields             = 4
	maxScheduleValues             = 3
	percentageScale               = 100
	maxLoggedPayloadBytes         = 128 << 10
	historyWriteAttempts          = 3
	historyRetryDelay             = 250 * time.Millisecond
	reinforcementWeekdayUnchanged = 255
	discordTimestampFull          = "F"
	discordTimestampRelative      = "R"
	allianceLogoURL               = "https://images.evetech.net/alliances/%s/logo?size=64"
	corporationLogoURL            = "https://images.evetech.net/corporations/%s/logo?size=64"
	eveWhoCharacterURL            = "https://evewho.com/character/%s"
	eveWhoCorporationURL          = "https://evewho.com/corporation/%s"
	eveWhoAllianceURL             = "https://evewho.com/alliance/%s"
	dotlanSystemURL               = "https://evemaps.dotlan.net/system/%s"
	dotlanRegionURL               = "https://evemaps.dotlan.net/region/%s"
)

var eventDescriptionTemplates = map[string]string{
	"StructureUnderAttack":                      "A structure is under attack in %s.",
	"StructureDestroyed":                        "A structure has been destroyed in %s.",
	"StructureAnchoring":                        "A structure has started anchoring in %s.",
	"StructureUnanchoring":                      "A structure has started unanchoring in %s.",
	"StructureVulnerable":                       "A structure is vulnerable in %s.",
	"StructureReinforced":                       "A structure has entered reinforced mode in %s.",
	"StructureOnline":                           "A structure has come online in %s.",
	"StructureWentLowPower":                     "A structure has entered low power mode in %s.",
	"StructureWentHighPower":                    "A structure has entered full power mode in %s.",
	"StructuresReinforcementChanged":            "The reinforcement schedule changed for structures in %s.",
	"StructureServicesOffline":                  "Services on a structure are offline in %s.",
	"StructureFuelAlert":                        "A structure has a fuel warning in %s.",
	"StructureLowReagentsAlert":                 "A structure has low reagents in %s.",
	"StructureNoReagentsAlert":                  "A structure has no reagents in %s.",
	"StructureLostShields":                      "A structure has lost its shields in %s.",
	"StructureLostArmor":                        "A structure has lost its armor in %s.",
	"StructureImpendingAbandonmentAssetsAtRisk": "A structure's assets are at risk of abandonment in %s.",
	"EntosisCaptureStarted":                     "Sovereignty entosis capture has started in %s.",
	"EntosisCaptureFinished":                    "Sovereignty entosis capture has finished in %s.",
	"EntosisCaptureNodesReinforced":             "Sovereignty entosis nodes have entered reinforced mode in %s.",
	"ESSMainBankLink":                           "The ESS main bank has been linked in %s.",
	"ESSReserveBankLink":                        "The ESS reserve bank has been linked in %s.",
	"SkyhookLostShields":                        "A skyhook has lost its shields and entered reinforced mode in %s.",
	"SkyhookDestroyed":                          "A skyhook has been destroyed in %s.",
	"SkyhookOnline":                             "A skyhook has come online in %s.",
	"SkyhookDeployed":                           "A skyhook has been deployed in %s.",
	"MercenaryDenReinforced":                    "A mercenary den has entered reinforced mode in %s.",
	"MercenaryDenAttacked":                      "A mercenary den is under attack in %s.",
	"MercenaryDenNewMTO":                        "A mercenary den has received a new tactical operation in %s.",
	"MoonminingExtractionStarted":               "A moon mining extraction has started in %s.",
	"MoonminingExtractionCancelled":             "A moon mining extraction was canceled in %s.",
	"MoonminingExtractionFinished":              "A moon mining extraction has finished in %s.",
	"MoonminingLaserFired":                      "A moon mining laser fired, and the extraction is ready for harvesting in %s.",
	"MoonminingAutomaticFracture":               "A moon mining extraction fractured automatically and is ready for harvesting in %s.",
	"OwnershipTransferred":                      "Structure ownership has transferred in %s.",
	"SovStationEnteredReinforce":                "A Sovereignty Hub has entered reinforced mode in %s.",
	"SovStationExitedReinforce":                 "A Sovereignty Hub has exited reinforced mode in %s.",
	"SovStructureReinforced":                    "A Sovereignty Hub has been reinforced in %s.",
	"SovStructureDestroyed":                     "A Sovereignty Hub has been destroyed in %s.",
	"SovStructureSelfDestructRequested":         "A Sovereignty Hub self-destruct has been requested in %s.",
	"SovStructureSelfDestructCancel":            "A Sovereignty Hub self-destruct was canceled in %s.",
	"SovStructureSelfDestructFinished":          "A Sovereignty Hub self-destruct has finished in %s.",
	"SovAllClaimAquiredMsg":                     "Sovereignty has been claimed in %s.",
	"SovereigntyClaimed":                        "Sovereignty has been claimed in %s.",
	"SovAllClaimLostMsg":                        "Sovereignty has been lost in %s.",
	"SovereigntyLost":                           "Sovereignty has been lost in %s.",
	"StationServiceEnabled":                     "A station service has been enabled in %s.",
	"StationServiceDisabled":                    "A station service has been disabled in %s.",
	"TowerAlertMsg":                             "A starbase is under attack in %s.",
	"TowerResourceAlertMsg":                     "A starbase has reported a resource shortage in %s.",
	"OrbitalAttacked":                           "A customs office is under attack in %s.",
	"OrbitalReinforced":                         "A customs office has entered reinforced mode in %s.",
}

var eventFieldRenderers = [...]func(*eventView) []discord.Field{
	corporationFields,
	ownershipFields,
	whenFields,
	attackerFields,
	actorFields,
	allianceFields,
	systemFields,
	regionFields,
	planetFields,
	structureFields,
	structureTypeFields,
	integrityFields,
	resourceFields,
	oreCompositionFields,
	timerFields,
	activityFields,
	decloakFields,
}

// Service resolves auth-next configuration and delivers notification embeds.
type Service struct {
	delivery    discord.Delivery
	enricher    enrichment.Enricher
	routing     *routing.Policy
	targets     map[string]webhookTarget
	logPayloads bool
	logger      *slog.Logger
	history     notificationstate.AlertHistory
}

// Destination is a named Discord destination and its canonical alert selector mapping.
type Destination struct {
	ID                      string
	WebhookURLs             []string
	WebhookTargets          []WebhookTarget
	AlertTypes              []string
	ExcludeAlertTypes       []string
	ExcludeStructureTypeIDs []string
	IncludeCorporationIDs   []string
	ExcludeCorporationIDs   []string
	Presentation            Presentation
	MentionRules            []MentionRule
	SenderName              string
	SenderAvatarURL         string
}

// WebhookTarget identifies one stable Discord webhook target.
type WebhookTarget struct {
	ID  string
	URL string
}

// Presentation controls provider-specific rendering preferences for a destination.
type Presentation struct {
	ShowEntityIDs bool
}

// MentionRule configures an optional Discord mention for matching alert selectors.
type MentionRule struct {
	AlertTypes []string
	Mention    string
}

// Config controls local alert routing and Discord delivery.
type Config struct {
	Destinations []Destination
	Enricher     enrichment.Enricher
	LogPayloads  bool
	Logger       *slog.Logger
	History      notificationstate.AlertHistory
}

// alertColors assigns a color to every canonical alert leaf and supported group.
// Unknown selectors intentionally default to danger.
var alertColors = map[string]int{
	// Structures.
	notifications.AlertStructureUnderAttack:           colorDanger,
	notifications.AlertStructureDestroyed:             colorDanger,
	notifications.AlertStructureAnchoring:             colorInformational,
	notifications.AlertStructureUnanchoring:           colorWarning,
	notifications.AlertStructureReinforced:            colorDanger,
	notifications.AlertStructureOnline:                colorSuccess,
	notifications.AlertStructureWentLowPower:          colorWarning,
	notifications.AlertStructureWentHighPower:         colorSuccess,
	notifications.AlertStructuresReinforcementChanged: colorWarning,
	notifications.AlertStructureVulnerable:            colorWarning,
	notifications.AlertStructureOffline:               colorWarning,
	notifications.AlertStructureOwnership:             colorInformational,
	notifications.AlertStructureFuelAlert:             colorWarning,
	notifications.AlertStructureLowReagents:           colorWarning,
	notifications.AlertStructureNoReagents:            colorDanger,
	notifications.AlertStructureLostShields:           colorDanger,
	notifications.AlertStructureLostArmor:             colorDanger,
	notifications.AlertStructureImpendingAbandonment:  colorDanger,

	// Sovereignty and ESS.
	notifications.AlertEntosisCaptureStarted:             colorDanger,
	notifications.AlertEntosisCaptureFinished:            colorDanger,
	notifications.AlertEntosisCaptureNodesReinforced:     colorDanger,
	notifications.AlertSovCommandNodeEventStarted:        colorDanger,
	notifications.AlertSovStationEnteredReinforce:        colorDanger,
	notifications.AlertSovStationExitedReinforce:         colorWarning,
	notifications.AlertSovStructureReinforced:            colorDanger,
	notifications.AlertSovStructureDestroyed:             colorDanger,
	notifications.AlertSovAllClaimAcquiredMsg:            colorSuccess,
	notifications.AlertSovAllClaimLostMsg:                colorDanger,
	notifications.AlertSovStructureSelfDestructRequested: colorDanger,
	notifications.AlertSovStructureSelfDestructCancel:    colorWarning,
	notifications.AlertSovStructureSelfDestructFinished:  colorDanger,
	notifications.AlertSovereigntyClaimed:                colorSuccess,
	notifications.AlertSovereigntyLost:                   colorDanger,
	notifications.AlertESSMainBankLink:                   colorDanger,
	notifications.AlertESSReserveBankLink:                colorWarning,

	// Skyhooks, mercenary dens, and moon mining.
	notifications.AlertSkyhookUnderAttack:            colorDanger,
	notifications.AlertSkyhookLostShields:            colorDanger,
	notifications.AlertSkyhookDestroyed:              colorDanger,
	notifications.AlertSkyhookOnline:                 colorInformational,
	notifications.AlertSkyhookDeployed:               colorInformational,
	notifications.AlertMercenaryDenReinforced:        colorDanger,
	notifications.AlertMercenaryDenAttacked:          colorDanger,
	notifications.AlertMercenaryDenNewMTO:            colorWarning,
	notifications.AlertMoonminingExtractionStarted:   colorInformational,
	notifications.AlertMoonminingExtractionCancelled: colorDanger,
	notifications.AlertMoonminingExtractionFinished:  colorDanger,
	notifications.AlertMoonminingLaserFired:          colorSuccess,
	notifications.AlertMoonminingAutomaticFracture:   colorSuccess,

	// Station services, starbases, and customs offices.
	notifications.AlertStationServiceEnabled:   colorInformational,
	notifications.AlertStationServiceDisabled:  colorWarning,
	notifications.AlertStarbaseUnderAttack:     colorDanger,
	notifications.AlertStarbaseResourceAlert:   colorWarning,
	notifications.AlertCustomsOfficeAttacked:   colorDanger,
	notifications.AlertCustomsOfficeReinforced: colorDanger,

	// Preserve warning behavior when a group selector is used directly.
	notifications.AlertESS:           colorWarning,
	notifications.AlertStructureFuel: colorWarning,
	notifications.AlertStarbase:      colorWarning,
}

// DeliveryRequest identifies one event and optionally limits delivery to the
// destinations that failed in a previous attempt.
type DeliveryRequest struct {
	Corporation         authnextdb.Corporation
	CharacterID         string
	RawNotificationJSON []byte
	Event               *notifications.Event
	DestinationIDs      []string
}

// DeliveryError reports the destinations that did not accept an alert.
type DeliveryError struct {
	DestinationIDs []string
	Err            error
}

// Error returns the aggregate destination delivery failure.
func (e *DeliveryError) Error() string {
	if e == nil || e.Err == nil {
		return "alert delivery failed"
	}
	return e.Err.Error()
}

// Unwrap exposes the aggregate delivery failure to errors.Is and errors.As.
func (e *DeliveryError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Database is the persistence subset needed to resolve structure names.
type Database interface {
	GetStructuresByIDs(context.Context, []string) ([]authnextdb.Structure, error)
}

// NewService creates the alert delivery service. The structure database may be
// nil; enrichment remains best-effort through the supplied enrichment service.
func NewService(database Database, discordDelivery discord.Delivery, config *Config) (*Service, error) {
	if discordDelivery == nil || config == nil {
		return nil, errors.New("alert service dependencies are required")
	}
	routingDestinations, targets := compileDiscordDestinations(config.Destinations)
	compiledRouting, err := routing.New(routingDestinations)
	if err != nil {
		return nil, err
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	enricher := config.Enricher
	if enricher == nil {
		enricher = enrichment.NewService(database, nil, logger)
	}
	return &Service{
		delivery:    discordDelivery,
		enricher:    enricher,
		routing:     compiledRouting,
		targets:     targets,
		logPayloads: config.LogPayloads,
		logger:      logger,
		history:     config.History,
	}, nil
}

// Deliver resolves configured destinations and sends an alert event.
func (s *Service) Deliver(ctx context.Context, request *DeliveryRequest) error {
	if ctx == nil {
		return errors.New("alert delivery context is required")
	}
	if request == nil || request.Event == nil {
		return errors.New("alert event is required")
	}
	if s == nil || s.routing == nil {
		return nil
	}
	candidates := s.routing.PreRoute(request.Corporation.ID, request.Event.AlertType)
	candidates = s.routing.Restrict(candidates, request.DestinationIDs)
	if len(candidates) == 0 {
		s.logger.Debug("alert has no configured destinations",
			"notification_id", request.Event.NotificationID,
			"alert_type", request.Event.AlertType,
		)
		return nil
	}
	view, err := s.buildEventView(ctx, request)
	if err != nil {
		return err
	}
	candidates = s.routing.PostRoute(view, candidates)
	if len(candidates) == 0 {
		return nil
	}
	targets := s.webhookTargets(candidates)
	if len(targets) == 0 {
		return nil
	}
	s.logDestinationsResolved(request, len(targets))
	return s.deliverTargets(ctx, view, targets)
}

// PreRoute selects candidate webhook targets using only the raw event and
// polling corporation context. It performs no enrichment or external I/O.
func (s *Service) PreRoute(envelope *enrichment.Envelope) []string {
	if s == nil || s.routing == nil || envelope == nil {
		return nil
	}
	return s.routing.PreRoute(envelope.Corporation.ID, envelope.Event.AlertType)
}

// PostRoute applies enrichment-dependent filters to pre-routed webhook targets.
func (s *Service) PostRoute(view *enrichment.Context, candidateIDs []string) []string {
	if s == nil || s.routing == nil || view == nil {
		return nil
	}
	return s.uniqueWebhookTargetIDs(s.routing.PostRoute(view, candidateIDs))
}

// Deliver sends one enriched alert through the configured Discord webhook
// target and converts provider-specific failures to a generic delivery result.
func (s *Service) DeliverTarget(ctx context.Context, view *enrichment.Context, targetID string) delivery.Outcome {
	if ctx == nil {
		return delivery.Outcome{Status: delivery.OutcomePermanent, Err: errors.New("alert delivery context is required")}
	}
	if s == nil || view == nil {
		return delivery.Outcome{Status: delivery.OutcomePermanent, Err: errors.New("alert target context is required")}
	}
	target := s.webhookTarget(targetID)
	if target == nil {
		return delivery.Outcome{Status: delivery.OutcomePermanent, Err: fmt.Errorf("unknown Discord target %q", targetID)}
	}
	if err := s.deliverTarget(ctx, target, view); err != nil {
		var retryErr *discord.RetryableError
		if errors.As(err, &retryErr) && retryErr != nil {
			return delivery.Outcome{Status: delivery.OutcomeRetryable, RetryAfter: retryErr.Delay, Err: err}
		}
		return delivery.Outcome{Status: delivery.OutcomePermanent, Err: err}
	}
	return delivery.Outcome{Status: delivery.OutcomeAccepted}
}

// RecordTerminalFailure records an enriched alert that could not be delivered.
func (s *Service) RecordTerminalFailure(ctx context.Context, view *enrichment.Context, destinationIDs []string, deliveryErr error) error {
	if ctx == nil {
		return errors.New("alert history context is required")
	}
	return s.recordFailureHistory(ctx, view, destinationIDs, deliveryErr)
}

func (s *Service) recordFailureHistory(ctx context.Context, view *eventView, destinationIDs []string, deliveryErr error) error {
	if s == nil || s.history == nil || view == nil {
		return nil
	}
	if deliveryErr == nil {
		return errors.New("delivery failure is required")
	}
	errorText := truncate(deliveryErr.Error(), maxTextSize)
	seen := make(map[string]struct{}, len(destinationIDs))
	var errs []error
	for _, destinationID := range destinationIDs {
		destinationID = strings.TrimSpace(destinationID)
		if destinationID == "" {
			continue
		}
		if _, ok := seen[destinationID]; ok {
			continue
		}
		seen[destinationID] = struct{}{}
		target := s.webhookTarget(destinationID)
		message := renderForTarget(view, target)
		if err := s.recordHistoryOutcome(ctx, destinationID, view, &message, notificationstate.AlertDeliveryStatusFailed, errorText); err != nil {
			errs = append(errs, fmt.Errorf("record failed alert for destination %s: %w", destinationID, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) deliverTargets(ctx context.Context, view *eventView, targets []webhookTarget) error {
	var errs []error
	failedIDs := make([]string, 0)
	for i := range targets {
		if err := s.deliverTarget(ctx, &targets[i], view); err != nil {
			errs = append(errs, err)
			failedIDs = append(failedIDs, targets[i].ID)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &DeliveryError{DestinationIDs: failedIDs, Err: errors.Join(errs...)}
}

func (s *Service) buildEventView(ctx context.Context, request *DeliveryRequest) (*eventView, error) {
	if request == nil || request.Event == nil {
		return nil, errors.New("alert event is required")
	}
	if s == nil {
		return nil, errors.New("alert enrichment service is unavailable")
	}
	if s.enricher == nil {
		return nil, errors.New("alert enrichment service is unavailable")
	}
	view, err := s.enricher.Enrich(ctx, &enrichment.Envelope{
		Corporation:         request.Corporation,
		CharacterID:         request.CharacterID,
		RawNotificationJSON: request.RawNotificationJSON,
		Event:               *request.Event,
	})
	if err != nil {
		return nil, err
	}
	return view, nil
}

type webhookTarget struct {
	ID              string
	URL             string
	Presentation    Presentation
	MentionRules    []MentionRule
	SenderName      string
	SenderAvatarURL string
}

func compileDiscordDestinations(destinations []Destination) (routingDestinations []routing.Destination, targets map[string]webhookTarget) {
	routingDestinations = make([]routing.Destination, 0, len(destinations))
	targets = make(map[string]webhookTarget)
	for index := range destinations {
		destination := &destinations[index]
		routingDestination := compileRoutingDestination(destination)
		compileWebhookTargets(destination, &routingDestination, targets)
		routingDestinations = append(routingDestinations, routingDestination)
	}
	return routingDestinations, targets
}

func compileRoutingDestination(destination *Destination) routing.Destination {
	if destination == nil {
		return routing.Destination{}
	}
	return routing.Destination{
		ID: destination.ID,
		Filters: routing.Filters{
			AlertTypes:              append([]string(nil), destination.AlertTypes...),
			ExcludeAlertTypes:       append([]string(nil), destination.ExcludeAlertTypes...),
			ExcludeStructureTypeIDs: append([]string(nil), destination.ExcludeStructureTypeIDs...),
			IncludeCorporationIDs:   append([]string(nil), destination.IncludeCorporationIDs...),
			ExcludeCorporationIDs:   append([]string(nil), destination.ExcludeCorporationIDs...),
		},
	}
}

func compileWebhookTargets(destination *Destination, routingDestination *routing.Destination, targets map[string]webhookTarget) {
	if destination == nil || routingDestination == nil || targets == nil {
		return
	}
	webhookTargets := destination.WebhookTargets
	if len(webhookTargets) == 0 {
		webhookTargets = webhookTargetsFromURLs(destination)
	}
	for targetIndex := range webhookTargets {
		target := &webhookTargets[targetIndex]
		webhookURL := strings.TrimRight(strings.TrimSpace(target.URL), "/")
		if webhookURL == "" {
			continue
		}
		targetID := strings.TrimSpace(target.ID)
		if targetID == "" {
			targetID = fmt.Sprintf("%s#%d", destination.ID, targetIndex+1)
		}
		routingDestination.TargetIDs = append(routingDestination.TargetIDs, targetID)
		targets[targetID] = webhookTarget{
			ID:              targetID,
			URL:             webhookURL,
			Presentation:    destination.Presentation,
			MentionRules:    append([]MentionRule(nil), destination.MentionRules...),
			SenderName:      destination.SenderName,
			SenderAvatarURL: destination.SenderAvatarURL,
		}
	}
}

func webhookTargetsFromURLs(destination *Destination) []WebhookTarget {
	if destination == nil {
		return nil
	}
	targets := make([]WebhookTarget, 0, len(destination.WebhookURLs))
	for targetIndex, rawURL := range destination.WebhookURLs {
		targetID := destination.ID
		if len(destination.WebhookURLs) > 1 {
			targetID = fmt.Sprintf("%s#%d", destination.ID, targetIndex+1)
		}
		targets = append(targets, WebhookTarget{ID: targetID, URL: rawURL})
	}
	return targets
}

func (s *Service) webhookTargets(targetIDs []string) []webhookTarget {
	if s == nil || s.routing == nil || len(s.targets) == 0 {
		return nil
	}
	targets := make([]webhookTarget, 0, len(targetIDs))
	seenURLs := make(map[string]struct{}, len(targetIDs))
	for _, targetID := range targetIDs {
		if target, ok := s.targets[targetID]; ok {
			if _, duplicate := seenURLs[target.URL]; duplicate {
				continue
			}
			seenURLs[target.URL] = struct{}{}
			targets = append(targets, target)
		}
	}
	return targets
}

func (s *Service) uniqueWebhookTargetIDs(targetIDs []string) []string {
	if s == nil || len(targetIDs) == 0 {
		return nil
	}
	selected := make([]string, 0, len(targetIDs))
	seenIDs := make(map[string]struct{}, len(targetIDs))
	seenURLs := make(map[string]struct{}, len(targetIDs))
	for _, targetID := range targetIDs {
		if _, duplicate := seenIDs[targetID]; duplicate {
			continue
		}
		seenIDs[targetID] = struct{}{}
		target, known := s.targets[targetID]
		if known {
			if _, duplicate := seenURLs[target.URL]; duplicate {
				continue
			}
			seenURLs[target.URL] = struct{}{}
		}
		selected = append(selected, targetID)
	}
	return selected
}

func (s *Service) webhookTarget(targetID string) *webhookTarget {
	if s == nil || s.routing == nil || len(s.targets) == 0 {
		return nil
	}
	target, ok := s.targets[targetID]
	if !ok {
		return nil
	}
	return &target
}

func (s *Service) deliverTarget(ctx context.Context, target *webhookTarget, view *eventView) error {
	if target == nil || view == nil {
		return errors.New("alert webhook target and event view are required")
	}
	s.logDestinationResolved(view, target.ID)
	message := renderForTarget(view, target)
	startedAt := time.Now()
	s.logger.Debug("Discord alert delivery started",
		"destination_id", target.ID,
		"notification_id", view.Event.NotificationID,
		"alert_type", view.Event.AlertType,
	)
	if err := s.delivery.Deliver(ctx, discord.Destination{ID: target.ID, WebhookURL: target.URL}, &message); err != nil {
		s.logger.Warn("Discord alert delivery failed",
			"destination_id", target.ID,
			"notification_id", view.Event.NotificationID,
			"duration", time.Since(startedAt),
			"err", err,
		)
		return fmt.Errorf("deliver destination %s: %w", target.ID, err)
	}
	if err := s.recordHistory(ctx, target.ID, view, &message); err != nil {
		s.logger.Warn("Discord alert history record failed",
			"destination_id", target.ID,
			"notification_id", view.Event.NotificationID,
			"err", err,
		)
	}
	s.logger.Info("Discord alert delivery completed",
		"destination_id", target.ID,
		"notification_id", view.Event.NotificationID,
		"duration", time.Since(startedAt),
	)
	return nil
}

func (s *Service) recordHistory(ctx context.Context, destinationID string, view *eventView, message *discord.Message) error {
	return s.recordHistoryOutcome(ctx, destinationID, view, message, notificationstate.AlertDeliveryStatusDelivered, "")
}

func (s *Service) recordHistoryOutcome(ctx context.Context, destinationID string, view *eventView, message *discord.Message, deliveryStatus, deliveryError string) error {
	if s == nil || s.history == nil || view == nil || message == nil {
		return nil
	}
	rawNotification := append([]byte(nil), view.RawNotificationJSON...)
	if len(rawNotification) == 0 {
		rawNotification = []byte("null")
	}
	eventJSON, err := json.Marshal(view.Event)
	if err != nil {
		return fmt.Errorf("encode classified event: %w", err)
	}
	discordPayload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode Discord payload: %w", err)
	}
	return recordHistoryWithRetry(ctx, s.history, &notificationstate.AlertHistoryRecord{
		NotificationID:      view.Event.NotificationID,
		NotificationType:    view.Event.NotificationType,
		AlertType:           view.Event.AlertType,
		CorporationID:       view.PollingCorporationID,
		CorporationName:     view.PollingCorporationName,
		CorporationTicker:   view.PollingCorporationTicker,
		CharacterID:         view.CharacterID,
		DestinationID:       destinationID,
		DeliveryStatus:      deliveryStatus,
		DeliveryError:       deliveryError,
		DispatchedAt:        time.Now().UTC(),
		RawNotificationJSON: rawNotification,
		ClassifiedEventJSON: eventJSON,
		DiscordPayloadJSON:  discordPayload,
	})
}

func recordHistoryWithRetry(ctx context.Context, history notificationstate.AlertHistory, record *notificationstate.AlertHistoryRecord) error {
	if ctx == nil {
		return errors.New("alert history context is required")
	}
	if history == nil {
		return nil
	}

	var lastErr error
	for attempt := 1; attempt <= historyWriteAttempts; attempt++ {
		lastErr = recordAlertSafely(ctx, history, record)
		if lastErr == nil {
			return nil
		}
		if attempt == historyWriteAttempts {
			break
		}
		if err := waitForHistoryRetry(ctx); err != nil {
			return err
		}
	}
	return lastErr
}

func waitForHistoryRetry(ctx context.Context) error {
	timer := time.NewTimer(historyRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func recordAlertSafely(ctx context.Context, history notificationstate.AlertHistory, record *notificationstate.AlertHistoryRecord) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = errors.New("alert history persistence panic")
		}
	}()
	return history.RecordAlert(ctx, record)
}

func (s *Service) logDestinationsResolved(request *DeliveryRequest, count int) {
	attrs := []any{
		"notification_id", request.Event.NotificationID,
		"alert_type", request.Event.AlertType,
		"destination_count", count,
	}
	if s.logPayloads && len(request.RawNotificationJSON) > 0 {
		attrs = append(attrs, "raw_payload", logPayload(request.RawNotificationJSON))
	}
	s.logger.Debug("alert destinations resolved", attrs...)
}

func (s *Service) logDestinationResolved(view *eventView, destinationID string) {
	if s == nil || s.logger == nil || view == nil {
		return
	}
	attrs := []any{
		"notification_id", view.Event.NotificationID,
		"alert_type", view.Event.AlertType,
		"destination_id", destinationID,
	}
	if s.logPayloads && len(view.RawNotificationJSON) > 0 {
		attrs = append(attrs, "raw_payload", logPayload(view.RawNotificationJSON))
	}
	s.logger.Debug("alert destination resolved", attrs...)
}

func logPayload(payload []byte) string {
	if len(payload) <= maxLoggedPayloadBytes {
		return string(payload)
	}
	return string(payload[:maxLoggedPayloadBytes]) + "...[truncated]"
}

type eventView = enrichment.Context

type reinforcementCoverageRegion struct {
	label   string
	systems map[string]reinforcementCoverageSystem
}

type reinforcementCoverageSystem struct {
	label string
	types map[string]reinforcementCoverageType
}

type reinforcementCoverageType struct {
	label string
	names []string
	count int
}

func render(view *eventView, senderName, avatarURL string) discord.Message {
	if view == nil {
		return discord.Message{}
	}
	title := escapeMarkdown(notifications.DisplayName(view.Event.NotificationType))
	description := eventDescription(view)
	description = truncate(description, maxTextSize)
	fields := buildFields(view)
	color := alertColor(&view.Event)
	author := eventAuthor(view)
	return discord.Message{
		Username:  senderName,
		AvatarURL: avatarURL,
		Embeds: []discord.Embed{{
			Title:       title,
			Description: description,
			Color:       color,
			Timestamp:   view.Event.Timestamp,
			Fields:      fields,
			Thumbnail:   structureThumbnail(view.Structures, view.Event.StructureTypeID),
			Author:      author,
		}},
		AllowedMentions: discord.AllowedMentions{Parse: []string{}},
	}
}

func renderForTarget(view *eventView, target *webhookTarget) discord.Message {
	if view == nil {
		return discord.Message{}
	}
	if target == nil {
		return render(view, "", "")
	}
	presentation := *view
	presentation.ShowEntityIDs = target.Presentation.ShowEntityIDs
	message := render(&presentation, target.SenderName, target.SenderAvatarURL)
	if mention := mentionFor(target.MentionRules, view.Event.AlertType); mention != "" {
		message.Content = mention
		message.AllowedMentions = discord.AllowedMentions{Parse: []string{"everyone"}}
	}
	return message
}

func mentionFor(rules []MentionRule, alertType string) string {
	bestSpecificity := -1
	mention := ""
	for index := range rules {
		rule := &rules[index]
		for _, selector := range rule.AlertTypes {
			if !notifications.AlertTypeInGroup(alertType, selector) && selector != alertType {
				continue
			}
			specificity := strings.Count(selector, ".") + 1
			if specificity > bestSpecificity {
				bestSpecificity = specificity
				mention = rule.Mention
			}
		}
	}
	switch mention {
	case "here":
		return "@here"
	case "everyone":
		return "@everyone"
	default:
		return ""
	}
}

func buildFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}

	fields := make([]discord.Field, 0, discord.MaxEmbedFields)
	for _, renderFields := range eventFieldRenderers {
		fields = append(fields, renderFields(view)...)
	}
	if len(fields) > discord.MaxEmbedFields {
		return fields[:discord.MaxEmbedFields]
	}
	return fields
}

func corporationFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	if !view.CorporationOwned {
		return nil
	}
	return []discord.Field{{Name: "Corporation", Value: corporationLabel(view.CorporationID, view.CorporationName, view.CorporationTicker, view.ShowEntityIDs), Inline: true}}
}

func ownershipFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	if view.Event.NotificationType != "OwnershipTransferred" {
		return nil
	}
	previousOwner := corporationLabel(view.Event.OldOwnerCorporationID, view.Event.OldOwnerCorporationName, "", view.ShowEntityIDs)
	newOwner := corporationLabel(view.Event.NewOwnerCorporationID, view.Event.NewOwnerCorporationName, "", view.ShowEntityIDs)
	if previousOwner == "" && newOwner == "" {
		return nil
	}
	lines := make([]string, 0, maxOwnershipParts)
	if previousOwner != "" {
		lines = append(lines, "Previous owner: "+previousOwner)
	}
	if newOwner != "" {
		lines = append(lines, "New owner: "+newOwner)
	}
	return []discord.Field{{Name: "Ownership", Value: strings.Join(lines, "\n"), Inline: false}}
}

func whenFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	return []discord.Field{{Name: "When", Value: timestampValue(view.Event.Timestamp), Inline: false}}
}

func attackerFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	lines := make([]string, 0, maxAttackerParts)
	if character := attackerIdentityLabel(view.Event.AttackerCharacterName, view.Event.AttackerCharacterID, "Character", view.ShowEntityIDs); character != "" {
		lines = append(lines, character)
	}
	if corporation := attackerIdentityLabel(view.Event.AttackerCorporationName, view.Event.AttackerCorporationID, "Corporation", view.ShowEntityIDs); corporation != "" {
		lines = append(lines, corporation)
	}
	if alliance := eveWhoLabel(eveWhoAllianceURL, "Alliance", view.Event.AttackerAllianceID, view.Event.AttackerAllianceName, view.ShowEntityIDs); alliance != "" {
		lines = append(lines, "Alliance: "+alliance)
	}
	if len(lines) == 0 {
		return nil
	}
	return []discord.Field{{Name: "Attacker", Value: strings.Join(lines, "\n"), Inline: true}}
}

func actorFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	actor := attackerIdentityLabel(view.Event.ActorCharacterName, view.Event.ActorCharacterID, "Character", view.ShowEntityIDs)
	if actor == "" {
		return nil
	}
	return []discord.Field{{Name: "Actor", Value: actor, Inline: true}}
}

func allianceFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	alliance := view.AllianceName
	if alliance == "" {
		alliance = view.Event.AttackerAllianceName
	}
	if view.Event.AllianceID == "" {
		alliance = escapeMarkdown(strings.TrimSpace(alliance))
	} else {
		alliance = eveWhoLabel(eveWhoAllianceURL, "Alliance", view.Event.AllianceID, alliance, view.ShowEntityIDs)
	}
	if alliance != "" {
		return []discord.Field{{Name: "Alliance", Value: alliance, Inline: true}}
	}
	return nil
}

func systemFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	if view.Event.NotificationType == "StructuresReinforcementChanged" && view.Event.SystemID == "" {
		return nil
	}
	system := view.SystemName
	if view.Event.SystemID == "" {
		system = escapeMarkdown(strings.TrimSpace(system))
	} else {
		system = dotlanLabel(dotlanSystemURL, "System", view.Event.SystemID, system, view.ShowEntityIDs)
	}
	if system != "" {
		return []discord.Field{{Name: "Solar System", Value: system, Inline: true}}
	}
	return nil
}

func regionFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	if view.Event.NotificationType == "StructuresReinforcementChanged" && view.Event.SystemID == "" {
		return nil
	}
	region := view.RegionName
	if view.RegionID == "" {
		region = escapeMarkdown(strings.TrimSpace(region))
	} else {
		region = dotlanLabel(dotlanRegionURL, "Region", view.RegionID, region, view.ShowEntityIDs)
	}
	if region != "" {
		return []discord.Field{{Name: "Region", Value: region, Inline: true}}
	}
	return nil
}

func reinforcementSystemLabels(view *eventView) []string {
	if view == nil {
		return nil
	}
	labels := make(map[string]string, len(view.Structures))
	for index := range view.Structures {
		structure := &view.Structures[index]
		id := strings.TrimSpace(structure.SystemID)
		name := optionalString(structure.SystemName)
		label := systemLabel(id, name, view.ShowEntityIDs)
		if label == "" {
			continue
		}
		key := id
		if key == "" {
			key = name
		}
		labels[key] = label
	}
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		out = append(out, label)
	}
	slices.Sort(out)
	return out
}

func reinforcementCoverageField(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	regions := reinforcementCoverageGroups(view)
	if len(regions) == 0 {
		return nil
	}
	regionKeys := make([]string, 0, len(regions))
	for regionKey := range regions {
		regionKeys = append(regionKeys, regionKey)
	}
	slices.Sort(regionKeys)
	lines := make([]string, 0)
	for _, regionKey := range regionKeys {
		region := regions[regionKey]
		lines = append(lines, "**"+region.label+"**")
		lines = append(lines, reinforcementCoverageSystemLines(region)...)
	}
	return []discord.Field{{Name: "Structure Coverage", Value: truncate(strings.Join(lines, "\n"), maxStructureSummarySize), Inline: false}}
}

func reinforcementCoverageGroups(view *eventView) map[string]*reinforcementCoverageRegion {
	structures := reinforcementCoverageStructures(view)
	regions := make(map[string]*reinforcementCoverageRegion, len(structures))
	for index := range structures {
		structure := &structures[index]
		regionKey, regionLabel, systemKey, systemLabel := reinforcementCoverageLocation(view, structure)
		region := regions[regionKey]
		if region == nil {
			region = &reinforcementCoverageRegion{label: regionLabel, systems: make(map[string]reinforcementCoverageSystem)}
			regions[regionKey] = region
		}
		system := region.systems[systemKey]
		if system.label == "" {
			system.label = systemLabel
			system.types = make(map[string]reinforcementCoverageType)
		}
		typeKey, typeLabel := reinforcementCoverageTypeLabel(view, structure)
		structureType := system.types[typeKey]
		structureType.label = typeLabel
		structureType.count++
		if name := reinforcementCoverageStructureName(view, structure); name != "" && !slices.Contains(structureType.names, name) {
			structureType.names = append(structureType.names, name)
			slices.Sort(structureType.names)
		}
		system.types[typeKey] = structureType
		region.systems[systemKey] = system
	}
	return regions
}

func reinforcementCoverageStructures(view *eventView) []authnextdb.Structure {
	if view == nil {
		return nil
	}
	structures := append([]authnextdb.Structure(nil), view.Structures...)
	if len(view.Event.StructureReferences) == 0 {
		return structures
	}
	known := make(map[string]struct{}, len(structures))
	for index := range structures {
		known[structures[index].ID] = struct{}{}
	}
	for _, reference := range view.Event.StructureReferences {
		if reference.ID == "" {
			continue
		}
		if _, ok := known[reference.ID]; ok {
			continue
		}
		name := strings.TrimSpace(reference.Name)
		structures = append(structures, authnextdb.Structure{
			ID:     reference.ID,
			Name:   &name,
			TypeID: strings.TrimSpace(reference.TypeID),
		})
		known[reference.ID] = struct{}{}
	}
	return structures
}

func reinforcementCoverageLocation(view *eventView, structure *authnextdb.Structure) (regionKey, regionDisplay, systemKey, systemDisplay string) {
	systemID := strings.TrimSpace(structure.SystemID)
	systemName := optionalString(structure.SystemName)
	systemDisplay = systemLabel(systemID, systemName, view.ShowEntityIDs)
	if systemDisplay == "" {
		systemDisplay = "Unknown system"
	}
	regionID := strings.TrimSpace(optionalString(structure.RegionID))
	regionName := optionalString(structure.RegionName)
	regionDisplay = regionLabel(regionID, regionName, view.ShowEntityIDs)
	if regionDisplay == "" {
		regionDisplay = "Unknown region"
	}
	regionKey = regionID
	if regionKey == "" {
		regionKey = regionDisplay
	}
	systemKey = systemID
	if systemKey == "" {
		systemKey = systemDisplay
	}
	return regionKey, regionDisplay, systemKey, systemDisplay
}

func reinforcementCoverageTypeLabel(view *eventView, structure *authnextdb.Structure) (key, label string) {
	typeID := strings.TrimSpace(structure.TypeID)
	if typeID == "" {
		for _, reference := range view.Event.StructureReferences {
			if reference.ID == structure.ID {
				typeID = strings.TrimSpace(reference.TypeID)
				break
			}
		}
	}
	label = ""
	if view.StructureTypeNames != nil {
		label = strings.TrimSpace(view.StructureTypeNames[typeID])
	}
	if label == "" {
		label = strings.TrimSpace(optionalString(structure.TypeName))
	}
	if label == "" && typeID != "" {
		label = "Structure type " + typeID
	}
	if label == "" {
		label = "Unknown structure type"
	}
	key = typeID
	if key == "" {
		key = label
	}
	return key, label
}

func reinforcementCoverageStructureName(view *eventView, structure *authnextdb.Structure) string {
	if view == nil || structure == nil {
		return ""
	}
	if name := optionalString(structure.Name); name != "" {
		return name
	}
	for _, reference := range view.Event.StructureReferences {
		if reference.ID == structure.ID {
			return strings.TrimSpace(reference.Name)
		}
	}
	return ""
}

func reinforcementCoverageSystemLines(region *reinforcementCoverageRegion) []string {
	if region == nil {
		return nil
	}
	systemKeys := make([]string, 0, len(region.systems))
	for systemKey := range region.systems {
		systemKeys = append(systemKeys, systemKey)
	}
	slices.Sort(systemKeys)
	lines := make([]string, 0, len(systemKeys))
	for _, systemKey := range systemKeys {
		system := region.systems[systemKey]
		lines = append(lines, "- **"+system.label+"**")
		typeKeys := reinforcementCoverageTypeKeys(system)
		for _, typeKey := range typeKeys {
			coverage := system.types[typeKey]
			noun := "structures"
			if coverage.count == 1 {
				noun = "structure"
			}
			lines = append(lines, fmt.Sprintf("  - **%s** (%d %s)", escapeMarkdown(coverage.label), coverage.count, noun))
			for _, name := range coverage.names {
				lines = append(lines, "    - "+escapeMarkdown(name))
			}
		}
	}
	return lines
}

func reinforcementCoverageTypeKeys(system reinforcementCoverageSystem) []string {
	keys := make([]string, 0, len(system.types))
	for typeKey := range system.types {
		keys = append(keys, typeKey)
	}
	slices.SortFunc(keys, func(left, right string) int {
		if comparison := strings.Compare(system.types[left].label, system.types[right].label); comparison != 0 {
			return comparison
		}
		return strings.Compare(left, right)
	})
	return keys
}

func planetFields(view *eventView) []discord.Field {
	if view == nil || !notifications.AlertTypeInGroup(view.Event.AlertType, notifications.AlertCustomsOffices) {
		return nil
	}
	planet := strings.TrimSpace(view.PlanetName)
	if planet == "" {
		if view.Event.PlanetID == "" {
			return nil
		}
		planet = "Planet " + view.Event.PlanetID
	} else {
		planet = escapeMarkdown(planet)
		if view.ShowEntityIDs && view.Event.PlanetID != "" {
			planet += " (" + escapeMarkdown(view.Event.PlanetID) + ")"
		}
	}
	return []discord.Field{{Name: "Planet", Value: planet, Inline: true}}
}

func structureFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	if view.Event.NotificationType == "StructuresReinforcementChanged" {
		return reinforcementCoverageField(view)
	}
	lines := make([]string, 0, len(view.Structures))
	for index := range view.Structures {
		lines = append(lines, formatStructure(&view.Structures[index], view))
	}
	if len(lines) == 0 {
		for _, structureID := range view.Event.StructureIDs {
			lines = append(lines, fallbackStructureLine(structureID, view))
		}
	}
	if structures := truncate(strings.Join(lines, "\n"), maxStructureSummarySize); structures != "" {
		return []discord.Field{{Name: structureFieldName(view.Event.NotificationType), Value: structures, Inline: false}}
	}
	return nil
}

func structureTypeFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	if view.Event.NotificationType == "StructuresReinforcementChanged" {
		return nil
	}
	structureType := strings.TrimSpace(view.StructureTypeName)
	if structureType == "" {
		if view.Event.StructureTypeID == "" {
			return nil
		}
		structureType = "Structure type " + view.Event.StructureTypeID
	} else {
		structureType = escapeMarkdown(structureType)
		if view.ShowEntityIDs && view.Event.StructureTypeID != "" {
			structureType += " (" + escapeMarkdown(view.Event.StructureTypeID) + ")"
		}
	}
	if structureType != "" {
		return []discord.Field{{Name: "Structure Type", Value: structureType, Inline: true}}
	}
	return nil
}

func integrityFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	values := make([]string, 0, maxIntegrityValues)
	if view.Event.ShieldPercentage != nil {
		values = append(values, fmt.Sprintf("Shields: **%.1f%%**", *view.Event.ShieldPercentage))
	}
	if view.Event.ArmorPercentage != nil {
		values = append(values, fmt.Sprintf("Armor: **%.1f%%**", *view.Event.ArmorPercentage))
	}
	if view.Event.HullPercentage != nil {
		values = append(values, fmt.Sprintf("Hull: **%.1f%%**", *view.Event.HullPercentage))
	}
	if view.Event.ShieldValue != nil {
		values = append(values, "Shield value: **"+formatIntegrityValue(*view.Event.ShieldValue)+"**")
	}
	if view.Event.ArmorValue != nil {
		values = append(values, "Armor value: **"+formatIntegrityValue(*view.Event.ArmorValue)+"**")
	}
	if view.Event.HullValue != nil {
		values = append(values, "Hull value: **"+formatIntegrityValue(*view.Event.HullValue)+"**")
	}
	if len(values) > 0 {
		return []discord.Field{{Name: "Integrity", Value: strings.Join(values, "\n"), Inline: true}}
	}
	return nil
}

func resourceFields(view *eventView) []discord.Field {
	if view == nil || len(view.Event.ResourceRequirements) == 0 {
		return nil
	}
	lines := make([]string, 0, len(view.Event.ResourceRequirements))
	for _, requirement := range view.Event.ResourceRequirements {
		if requirement.TypeID == "" || requirement.Quantity <= 0 {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s x Type %s", formatInteger(requirement.Quantity), escapeMarkdown(requirement.TypeID)))
	}
	if len(lines) == 0 {
		return nil
	}
	return []discord.Field{{Name: "Resources Needed", Value: strings.Join(lines, "\n"), Inline: false}}
}

func oreCompositionFields(view *eventView) []discord.Field {
	if view == nil || len(view.Event.OreComposition) == 0 {
		return nil
	}
	totalVolume := 0.0
	for _, ore := range view.Event.OreComposition {
		if ore.Volume > 0 {
			totalVolume += ore.Volume
		}
	}
	if totalVolume <= 0 {
		return nil
	}
	lines := make([]string, 0, len(view.Event.OreComposition))
	for _, ore := range view.Event.OreComposition {
		if ore.Volume <= 0 {
			continue
		}
		name := strings.TrimSpace(view.OreTypeNames[ore.TypeID])
		if name == "" {
			name = "Ore type " + ore.TypeID
		}
		lines = append(lines, fmt.Sprintf("• %s: **%.1f%%**", escapeMarkdown(name), ore.Volume/totalVolume*percentageScale))
	}
	if len(lines) == 0 {
		return nil
	}
	return []discord.Field{{Name: "Ore Composition", Value: strings.Join(lines, "\n"), Inline: false}}
}

func decloakFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	if view.Event.DecloakAt.IsZero() {
		return nil
	}
	return []discord.Field{{Name: "Decloak At", Value: timestampValue(view.Event.DecloakAt), Inline: true}}
}

func structureFieldName(notificationType string) string {
	if notificationType == "OwnershipTransferred" {
		return "Structure"
	}
	return "Structures"
}

func eventAuthor(view *eventView) *discord.Author {
	if view == nil {
		return nil
	}
	if view.CorporationOwned {
		author := &discord.Author{Name: escapeMarkdown(view.CorporationName)}
		if view.CorporationID != "" {
			author.IconURL = fmt.Sprintf(corporationLogoURL, view.CorporationID)
		}
		return author
	}
	if view.Event.AllianceID != "" {
		name := view.AllianceName
		if name == "" {
			name = view.Event.AllianceName
		}
		if name == "" {
			return nil
		}
		iconURL := view.AllianceIconURL
		if iconURL == "" {
			iconURL = fmt.Sprintf(allianceLogoURL, view.Event.AllianceID)
		}
		return &discord.Author{
			Name:    escapeMarkdown(name),
			IconURL: iconURL,
		}
	}
	return nil
}

func alertColor(event *notifications.Event) int {
	if event == nil {
		return colorDanger
	}
	if color, ok := alertColors[event.AlertType]; ok {
		return color
	}
	return colorDanger
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func formatInteger(value int64) string {
	return strconv.FormatInt(value, 10)
}

func formatIntegrityValue(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func corporationLabel(id, name, ticker string, showEntityIDs bool) string {
	label := eveWhoLabel(eveWhoCorporationURL, "Corporation", id, name, showEntityIDs)
	if ticker == "" {
		return label
	}
	return label + " [" + escapeMarkdown(ticker) + "]"
}

func timestampValue(timestamp time.Time) string {
	return discordTimestamp(timestamp, discordTimestampFull) + "\nEVE Time: " + eveTime(timestamp)
}

func timeRemainingValue(start time.Time, remaining time.Duration) string {
	if remaining <= 0 || start.IsZero() {
		return ""
	}
	expires := start.Add(remaining)
	return discordTimestamp(expires, discordTimestampRelative) + "\nEVE Time: " + eveTime(expires)
}

func discordTimestamp(timestamp time.Time, style string) string {
	return fmt.Sprintf("<t:%d:%s>", timestamp.Unix(), style)
}

func eveTime(timestamp time.Time) string {
	return timestamp.UTC().Format("15:04:05 UTC")
}

func systemLabel(systemID, systemName string, showEntityIDs bool) string {
	if systemID == "" {
		return escapeMarkdown(strings.TrimSpace(systemName))
	}
	return dotlanLabel(dotlanSystemURL, "System", systemID, systemName, showEntityIDs)
}

func regionLabel(regionID, regionName string, showEntityIDs bool) string {
	if regionID == "" {
		return escapeMarkdown(strings.TrimSpace(regionName))
	}
	return dotlanLabel(dotlanRegionURL, "Region", regionID, regionName, showEntityIDs)
}

func fallbackStructureLine(structureID string, view *eventView) string {
	if view == nil {
		return ""
	}
	systemName := view.SystemName
	if systemName == "" {
		systemName = "unknown system"
	}
	structureName := view.Event.StructureName
	if structureName == "" {
		structureName = "Structure " + structureID
	}
	line := escapeMarkdown(structureName) + " in " + escapeMarkdown(systemName)
	if view.PlanetName != "" {
		line += "\nPlanet: " + escapeMarkdown(view.PlanetName)
	}
	if view.MoonName != "" {
		line += "\nMoon: " + escapeMarkdown(view.MoonName)
	}
	return line
}

func eventDescription(view *eventView) string {
	if view == nil {
		return ""
	}
	event := &view.Event
	system := systemLabel(event.SystemID, view.SystemName, view.ShowEntityIDs)
	if system == "" {
		system = "the reported system"
	}
	if event.NotificationType == "SkyhookUnderAttack" {
		return "A skyhook is under attack in " + system + "."
	}
	if event.NotificationType == "StructuresReinforcementChanged" && event.ReinforcedStructureCount != nil {
		systems := reinforcementSystemLabels(view)
		if len(systems) > 1 {
			return fmt.Sprintf("The reinforcement schedule changed for %d structures across %d solar systems.", *event.ReinforcedStructureCount, len(systems))
		}
		if len(systems) == 1 {
			return fmt.Sprintf("The reinforcement schedule changed for %d structures in %s.", *event.ReinforcedStructureCount, systems[0])
		}
		return fmt.Sprintf("The reinforcement schedule changed for %d structures across the reported solar systems.", *event.ReinforcedStructureCount)
	}
	if template, ok := eventDescriptionTemplates[event.NotificationType]; ok {
		return fmt.Sprintf(template, system)
	}
	if summary := strings.TrimSpace(event.Summary); summary != "" {
		return escapeMarkdown(summary)
	}
	return escapeMarkdown(notifications.DisplayName(event.NotificationType)) + "."
}

func isSkyhookReinforced(event *notifications.Event) bool {
	return event != nil && event.NotificationType == "SkyhookLostShields"
}

func timerFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	event := &view.Event
	fields := make([]discord.Field, 0, maxTimerFields)
	if !event.ReinforcedUntil.IsZero() {
		fields = append(fields, discord.Field{Name: "Reinforced Until", Value: timestampValue(event.ReinforcedUntil), Inline: true})
	}
	if remaining := timeRemainingValue(event.Timestamp, event.TimeLeft); remaining != "" {
		fieldName := "Time Remaining"
		if isSkyhookReinforced(event) && len(fields) == 0 {
			fieldName = "Reinforced Until"
		}
		fields = append(fields, discord.Field{Name: fieldName, Value: remaining, Inline: true})
	}
	if vulnerable := timeRemainingValue(event.Timestamp, event.VulnerableFor); vulnerable != "" && !isSkyhookReinforced(event) {
		fields = append(fields, discord.Field{Name: "Vulnerable Until", Value: vulnerable, Inline: true})
	}
	return fields
}

func activityFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	event := &view.Event
	fields := make([]discord.Field, 0, maxActivityFields)
	if !event.ReadyAt.IsZero() {
		fields = append(fields, discord.Field{Name: "Ready At", Value: timestampValue(event.ReadyAt), Inline: true})
	}
	if !event.AutoAt.IsZero() {
		fields = append(fields, discord.Field{Name: "Automatic Fracture At", Value: timestampValue(event.AutoAt), Inline: true})
	}
	if !event.DestructAt.IsZero() {
		fields = append(fields, discord.Field{Name: "Self-Destruct At", Value: timestampValue(event.DestructAt), Inline: true})
	}
	if event.AbandonAfterDays > 0 {
		fields = append(fields, discord.Field{Name: "Assets at Risk", Value: fmt.Sprintf("in %d days", event.AbandonAfterDays), Inline: true})
	}
	if event.ReinforcementHour != nil || event.ReinforcementWeekday != nil || event.ReinforcedStructureCount != nil {
		lines := make([]string, 0, maxScheduleValues)
		if event.ReinforcedStructureCount != nil {
			lines = append(lines, fmt.Sprintf("Structures: %d", *event.ReinforcedStructureCount))
		}
		if event.ReinforcementWeekday != nil {
			lines = append(lines, "Weekday: "+reinforcementWeekdayLabel(*event.ReinforcementWeekday))
		}
		if event.ReinforcementHour != nil {
			lines = append(lines, fmt.Sprintf("Hour: %02d:00 EVE", *event.ReinforcementHour))
		}
		fields = append(fields, discord.Field{Name: "Reinforcement Schedule", Value: strings.Join(lines, "\n"), Inline: true})
	}
	return fields
}

func reinforcementWeekdayLabel(weekday int) string {
	if weekday == reinforcementWeekdayUnchanged {
		return "Unchanged"
	}
	if weekday < 0 || weekday >= 7 {
		return fmt.Sprintf("Unknown (%d)", weekday)
	}
	return [...]string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}[weekday]
}

func attackerIdentityLabel(name, id, fallback string, showEntityIDs bool) string {
	if fallback == "Character" {
		return eveWhoLabel(eveWhoCharacterURL, fallback, id, name, showEntityIDs)
	}
	return eveWhoLabel(eveWhoCorporationURL, fallback, id, name, showEntityIDs)
}

func formatStructure(structure *authnextdb.Structure, view *eventView) string {
	if structure == nil || view == nil {
		return ""
	}
	name := optionalString(structure.Name)
	if name == "" {
		name = view.Event.StructureName
	}
	if name == "" {
		name = "Structure " + structure.ID
	}
	system := systemLabel(structure.SystemID, optionalString(structure.SystemName), view.ShowEntityIDs)
	if system == "" {
		system = "System " + structure.SystemID
	}
	if region := regionLabel(optionalString(structure.RegionID), optionalString(structure.RegionName), view.ShowEntityIDs); region != "" {
		system += " / " + region
	}
	line := escapeMarkdown(name) + " in " + system
	if structure.PlanetName != nil && *structure.PlanetName != "" {
		line += "\nPlanet: " + escapeMarkdown(*structure.PlanetName)
	} else if view.PlanetName != "" {
		line += "\nPlanet: " + escapeMarkdown(view.PlanetName)
	}
	if structure.MoonName != nil && *structure.MoonName != "" {
		line += "\nMoon: " + escapeMarkdown(*structure.MoonName)
	} else if view.MoonName != "" {
		line += "\nMoon: " + escapeMarkdown(view.MoonName)
	}
	return line
}

func structureThumbnail(structures []authnextdb.Structure, fallbackTypeID string) *discord.Image {
	typeID := fallbackTypeID
	if typeID == "" && len(structures) == 1 && structures[0].TypeID != "" {
		typeID = structures[0].TypeID
	}
	if typeID == "" {
		return nil
	}
	return &discord.Image{URL: "https://images.evetech.net/types/" + typeID + "/render?size=64"}
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-3]) + "..."
}

func eveWhoLabel(template, fallback, id, name string, showEntityIDs bool) string {
	return linkedLabel(template, fallback, id, name, false, showEntityIDs)
}

func dotlanLabel(template, fallback, id, name string, showEntityIDs bool) string {
	return linkedLabel(template, fallback, id, name, true, showEntityIDs)
}

func linkedLabel(template, fallback, id, name string, dotlan, showEntityIDs bool) string {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if id == "" {
		return escapeMarkdown(name)
	}
	label := name
	if label == "" {
		label = fallback + " " + id
	}
	pathID := id
	if dotlan && name != "" {
		pathID = strings.Join(strings.Fields(name), "_")
	}
	linked := fmt.Sprintf("[%s](%s)", escapeMarkdown(label), fmt.Sprintf(template, url.PathEscape(pathID)))
	if name == "" {
		return linked
	}
	if !showEntityIDs {
		return linked
	}
	return linked + " (" + escapeMarkdown(id) + ")"
}

func escapeMarkdown(value string) string {
	return strings.NewReplacer(
		"\\", "\\\\",
		"*", "\\*",
		"_", "\\_",
		"~", "\\~",
		"`", "\\`",
		">", "\\>",
		"|", "\\|",
		"[", "\\[",
		"]", "\\]",
	).Replace(value)
}
