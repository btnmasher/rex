// Package notifications classifies and sanitizes ESI character notifications.
package notifications

import (
	"html"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/btnmasher/rex/internal/esi"
)

const (
	AlertStructures                        = "structures"
	AlertStructureCombat                   = "structures.combat"
	AlertStructureState                    = "structures.state"
	AlertStructureOwnershipGroup           = "structures.ownership"
	AlertStructureFuel                     = "structures.resources"
	AlertStructureIntegrity                = "structures.integrity"
	AlertStructureAbandonment              = "structures.abandonment"
	AlertStructureUnderAttack              = "structures.combat.under_attack"
	AlertStructureDestroyed                = "structures.combat.destroyed"
	AlertStructureAnchoring                = "structures.state.anchoring"
	AlertStructureUnanchoring              = "structures.state.unanchoring"
	AlertStructureReinforced               = "structures.state.reinforced"
	AlertStructureOnline                   = "structures.state.online"
	AlertStructureWentLowPower             = "structures.state.low_power"
	AlertStructureWentHighPower            = "structures.state.high_power"
	AlertStructuresReinforcementChanged    = "structures.state.reinforcement_changed"
	AlertStructureVulnerable               = "structures.state.vulnerable"
	AlertStructureOffline                  = "structures.state.services_offline"
	AlertStructureOwnership                = "structures.ownership.transferred"
	AlertStructureFuelAlert                = "structures.resources.fuel_alert"
	AlertStructureLowReagents              = "structures.resources.low_reagents"
	AlertStructureNoReagents               = "structures.resources.no_reagents"
	AlertStructureLostShields              = "structures.integrity.lost_shields"
	AlertStructureLostArmor                = "structures.integrity.lost_armor"
	AlertStructureImpendingAbandonment     = "structures.abandonment.impending_assets_at_risk"
	AlertSovereignty                       = "sovereignty"
	AlertSovereigntyEntosis                = "sovereignty.entosis"
	AlertSovereigntyEvents                 = "sovereignty.events"
	AlertEntosisCaptureStarted             = "sovereignty.entosis.capture_started"
	AlertEntosisCaptureFinished            = "sovereignty.entosis.capture_finished"
	AlertEntosisCaptureNodesReinforced     = "sovereignty.entosis.capture_nodes_reinforced"
	AlertESS                               = "ess"
	AlertESSMainBankLink                   = "ess.main_bank_link"
	AlertESSReserveBankLink                = "ess.reserve_bank_link"
	AlertSkyhook                           = "skyhooks"
	AlertSkyhookUnderAttack                = "skyhooks.under_attack"
	AlertSkyhookLostShields                = "skyhooks.lost_shields"
	AlertSkyhookDestroyed                  = "skyhooks.destroyed"
	AlertSkyhookOnline                     = "skyhooks.online"
	AlertSkyhookDeployed                   = "skyhooks.deployed"
	AlertMercenaryDen                      = "mercenary_dens"
	AlertMercenaryDenReinforced            = "mercenary_dens.reinforced"
	AlertMercenaryDenAttacked              = "mercenary_dens.attacked"
	AlertMercenaryDenNewMTO                = "mercenary_dens.new_mto"
	AlertMoonmining                        = "moonmining"
	AlertMoonminingExtractionStarted       = "moonmining.extraction_started"
	AlertMoonminingExtractionCancelled     = "moonmining.extraction_canceled"
	AlertMoonminingExtractionFinished      = "moonmining.extraction_finished"
	AlertMoonminingLaserFired              = "moonmining.laser_fired"
	AlertMoonminingAutomaticFracture       = "moonmining.automatic_fracture"
	AlertSovCommandNodeEventStarted        = "sovereignty.events.command_node_event_started"
	AlertSovStationEnteredReinforce        = "sovereignty.events.station_entered_reinforce"
	AlertSovStationExitedReinforce         = "sovereignty.events.station_exited_reinforce"
	AlertSovStructureReinforced            = "sovereignty.events.structure_reinforced"
	AlertSovStructureDestroyed             = "sovereignty.events.structure_destroyed"
	AlertSovAllClaimAcquiredMsg            = "sovereignty.events.all_claim_acquired"
	AlertSovAllClaimLostMsg                = "sovereignty.events.all_claim_lost"
	AlertSovStructureSelfDestructRequested = "sovereignty.events.self_destruct_requested"
	AlertSovStructureSelfDestructCancel    = "sovereignty.events.self_destruct_canceled"
	AlertSovStructureSelfDestructFinished  = "sovereignty.events.self_destruct_finished"
	AlertSovereigntyClaimed                = "sovereignty.events.claimed"
	AlertSovereigntyLost                   = "sovereignty.events.lost"
	AlertStationServices                   = "station_services"
	AlertStationServiceEnabled             = "station_services.enabled"
	AlertStationServiceDisabled            = "station_services.disabled"
	AlertStarbase                          = "starbase"
	AlertStarbaseUnderAttack               = "starbase.under_attack"
	AlertStarbaseResourceAlert             = "starbase.resource_alert"
	AlertCustomsOffices                    = "customs_offices"
	AlertCustomsOfficeAttacked             = "customs_offices.attacked"
	AlertCustomsOfficeReinforced           = "customs_offices.reinforced"
	regexSubmatchCount                     = 2
	maxEVEIntervalTicks                    = (1<<63 - 1) / int64(eveTickDuration)
	maxStructureIDs                        = 16
	maxBulkStructureIDs                    = 64
	maxResourceRequirements                = 16
	eveTickDuration                        = 100 * time.Nanosecond
	eveEpochOffsetSeconds                  = 11644473600
	eveTicksPerSecond                      = int64(time.Second / eveTickDuration)
	eveNanosecondsPerTick                  = int64(eveTickDuration)
)

var (
	structureIDPattern         = regexp.MustCompile(`(?i)(?:structure[_ -]?id|structureid)\D{0,24}(\d{5,})`)
	bulkStructureIDPattern     = regexp.MustCompile(`(?:^|\s)-\s*-\s*(\d{5,})(?:\s|$)`)
	systemIDPattern            = regexp.MustCompile(`(?i)(?:solar[_ -]?system[_ -]?id|system[_ -]?id|solarsystemid)\D{0,24}(\d{5,})`)
	numberPattern              = regexp.MustCompile(`\d+`)
	selectorSegmentPattern     = regexp.MustCompile(`^[a-z0-9_]+$`)
	metadataKeyPattern         = regexp.MustCompile(`(?i)(?:aggressorAllianceID|aggressorAllianceName|aggressorCharacterID|aggressorCorpID|aggressorCorporationName|aggressorID|allStructureInfo|allianceID|allianceName|armorPercentage|armorValue|assetSafetyDurationFull|assetSafetyDurationMinimum|assetSafetyFullTimestamp|assetSafetyMinimumTimestamp|autoTime|campaignEventType|cancelledBy|charID|charName|characterID|characterName|corpID|corpLinkData|corpName|daysUntilAbandon|decloakTime|destructTime|firedBy|hullPercentage|hullValue|hour|isActive|isCorpOwned|itemID|listOfServiceModuleIDs|listOfTypesAndQty|mercenaryDenShowInfoData|moonID|moonLink|newOwnerCorpID|newOwnerCorpName|newStationID|numStructures|oldOwnerCorpID|oldOwnerCorpName|oreVolumeByType|ownerCorpLinkData|ownerCorpName|planetID|planetTypeID|readyTime|reinforceExitTime|shieldLevel|shieldPercentage|shieldValue|solarSystemID|solarSystemLink|solarsystemID|startedBy|startedByLink|structureID|structureLink|structureName|structureShowInfoData|structureTypeID|structuresReinforcementChanged|timeLeft|timestamp|timestampEntered|timestampExited|typeID|vulnerableTime|weekday|wants)\s*:`)
	resourceRequirementPattern = regexp.MustCompile(`(?i)quantity\s*:\s*(\d+)\s+typeID\s*:\s*(\d+)`)
	tagPattern                 = regexp.MustCompile(`<[^>]+>`)
	spacePattern               = regexp.MustCompile(`\s+`)
)

var displayNames = map[string]string{
	"StructureUnderAttack":                      "Structure Under Attack",
	"StructureDestroyed":                        "Structure Destroyed",
	"StructureAnchoring":                        "Structure Anchoring",
	"StructureUnanchoring":                      "Structure Unanchoring",
	"StructureVulnerable":                       "Structure Vulnerable",
	"StructureReinforced":                       "Structure Reinforced",
	"StructureOnline":                           "Structure Online",
	"StructureWentLowPower":                     "Structure Low Power",
	"StructureWentHighPower":                    "Structure Full Power",
	"StructuresReinforcementChanged":            "Structure Reinforcement Schedule Changed",
	"StructureServicesOffline":                  "Structure Services Offline",
	"StructureFuelAlert":                        "Structure Fuel Alert",
	"StructureLowReagentsAlert":                 "Structure Low Reagents",
	"StructureNoReagentsAlert":                  "Structure Out of Reagents",
	"StructureLostShields":                      "Structure Lost Shields",
	"StructureLostArmor":                        "Structure Lost Armor",
	"StructureImpendingAbandonmentAssetsAtRisk": "Structure Abandonment Risk",
	"EntosisCaptureStarted":                     "Entosis Capture Started",
	"EntosisCaptureFinished":                    "Entosis Capture Finished",
	"EntosisCaptureNodesReinforced":             "Entosis Capture Nodes Reinforced",
	"ESSMainBankLink":                           "ESS Main Bank Link",
	"ESSReserveBankLink":                        "ESS Reserve Bank Link",
	"SkyhookUnderAttack":                        "Skyhook Under Attack",
	"SkyhookLostShields":                        "Skyhook Lost Shields",
	"SkyhookDestroyed":                          "Skyhook Destroyed",
	"SkyhookOnline":                             "Skyhook Online",
	"SkyhookDeployed":                           "Skyhook Deployed",
	"MercenaryDenReinforced":                    "Mercenary Den Reinforced",
	"MercenaryDenAttacked":                      "Mercenary Den Under Attack",
	"MercenaryDenNewMTO":                        "Mercenary Den Tactical Operation",
	"MoonminingExtractionStarted":               "Moon Mining Extraction Started",
	"MoonminingExtractionCancelled":             "Moon Mining Extraction Canceled",
	"MoonminingExtractionFinished":              "Moon Mining Extraction Finished",
	"MoonminingLaserFired":                      "Moon Mining Laser Fired",
	"MoonminingAutomaticFracture":               "Moon Mining Automatic Fracture",
	"OwnershipTransferred":                      "Structure Ownership Transferred",
	"SovCommandNodeEventStarted":                "Sovereignty Hub Command Node Event Started",
	"SovStationEnteredReinforce":                "Sovereignty Hub Entered Reinforce",
	"SovStationExitedReinforce":                 "Sovereignty Hub Exited Reinforce",
	"SovStructureReinforced":                    "Sovereignty Hub Reinforced",
	"SovStructureDestroyed":                     "Sovereignty Hub Destroyed",
	"SovStructureSelfDestructRequested":         "Sovereignty Hub Self-Destruct Requested",
	"SovStructureSelfDestructCancel":            "Sovereignty Hub Self-Destruct Canceled",
	"SovStructureSelfDestructFinished":          "Sovereignty Hub Self-Destruct Finished",
	"SovAllClaimAquiredMsg":                     "Sovereignty Claimed",
	"SovAllClaimLostMsg":                        "Sovereignty Lost",
	"SovereigntyClaimed":                        "Sovereignty Claimed",
	"SovereigntyLost":                           "Sovereignty Lost",
	"StationServiceEnabled":                     "Station Service Enabled",
	"StationServiceDisabled":                    "Station Service Disabled",
	"TowerAlertMsg":                             "Starbase Under Attack",
	"TowerResourceAlertMsg":                     "Starbase Resource Alert",
	"OrbitalAttacked":                           "Customs Office Under Attack",
	"OrbitalReinforced":                         "Customs Office Reinforced",
}

// ResourceRequirement identifies a resource quantity reported by a starbase alert.
type ResourceRequirement struct {
	Quantity int64  `json:"quantity"`
	TypeID   string `json:"type_id"`
}

// Event is a classified notification ready for enrichment and delivery.
type Event struct {
	NotificationID           int64                 `json:"notification_id"`
	NotificationType         string                `json:"notification_type"`
	SenderID                 int64                 `json:"sender_id"`
	SenderType               string                `json:"sender_type"`
	Timestamp                time.Time             `json:"timestamp"`
	AlertType                string                `json:"alert_type"`
	StructureIDs             []string              `json:"structure_ids"`
	SystemID                 string                `json:"system_id"`
	AllianceID               string                `json:"alliance_id"`
	AllianceName             string                `json:"alliance_name"`
	AttackerAllianceID       string                `json:"attacker_alliance_id"`
	AttackerCharacterID      string                `json:"attacker_character_id"`
	AttackerCharacterName    string                `json:"attacker_character_name"`
	AttackerAllianceName     string                `json:"attacker_alliance_name"`
	AttackerCorporationID    string                `json:"attacker_corporation_id"`
	AttackerCorporationName  string                `json:"attacker_corporation_name"`
	OwnerCorporationID       string                `json:"owner_corporation_id"`
	OwnerCorporationName     string                `json:"owner_corporation_name"`
	NewOwnerCorporationID    string                `json:"new_owner_corporation_id"`
	NewOwnerCorporationName  string                `json:"new_owner_corporation_name"`
	OldOwnerCorporationID    string                `json:"old_owner_corporation_id"`
	OldOwnerCorporationName  string                `json:"old_owner_corporation_name"`
	StructureName            string                `json:"structure_name"`
	PlanetID                 string                `json:"planet_id"`
	MoonID                   string                `json:"moon_id"`
	StructureTypeID          string                `json:"structure_type_id"`
	TimeLeft                 time.Duration         `json:"time_left"`
	VulnerableFor            time.Duration         `json:"vulnerable_for"`
	DecloakAt                time.Time             `json:"decloak_at"`
	ReinforcedUntil          time.Time             `json:"reinforced_until"`
	ReadyAt                  time.Time             `json:"ready_at"`
	AutoAt                   time.Time             `json:"auto_at"`
	DestructAt               time.Time             `json:"destruct_at"`
	AbandonAfterDays         int                   `json:"abandon_after_days"`
	ActorCharacterID         string                `json:"actor_character_id"`
	ActorCharacterName       string                `json:"actor_character_name"`
	ReinforcementHour        *int                  `json:"reinforcement_hour"`
	ReinforcementWeekday     *int                  `json:"reinforcement_weekday"`
	ReinforcedStructureCount *int                  `json:"reinforced_structure_count"`
	ShieldPercentage         *float64              `json:"shield_percentage"`
	ArmorPercentage          *float64              `json:"armor_percentage"`
	HullPercentage           *float64              `json:"hull_percentage"`
	ShieldValue              *float64              `json:"shield_value"`
	ArmorValue               *float64              `json:"armor_value"`
	HullValue                *float64              `json:"hull_value"`
	ResourceRequirements     []ResourceRequirement `json:"resource_requirements"`
	IsActive                 *bool                 `json:"is_active"`
	Summary                  string                `json:"summary"`
	Text                     string                `json:"text"`
}

type alertDefinition struct {
	group string
	leaf  string
}

var alertGroups = map[string][]string{
	AlertStructures: {
		AlertStructureUnderAttack, AlertStructureDestroyed,
		AlertStructureAnchoring, AlertStructureUnanchoring, AlertStructureReinforced,
		AlertStructureOnline, AlertStructureWentLowPower, AlertStructureWentHighPower,
		AlertStructuresReinforcementChanged, AlertStructureVulnerable, AlertStructureOffline,
		AlertStructureOwnership, AlertStructureFuelAlert, AlertStructureLowReagents,
		AlertStructureNoReagents, AlertStructureLostShields, AlertStructureLostArmor,
		AlertStructureImpendingAbandonment,
	},
	AlertStructureCombat: {AlertStructureUnderAttack, AlertStructureDestroyed},
	AlertStructureState: {
		AlertStructureAnchoring, AlertStructureUnanchoring, AlertStructureReinforced,
		AlertStructureOnline, AlertStructureWentLowPower, AlertStructureWentHighPower,
		AlertStructuresReinforcementChanged, AlertStructureVulnerable, AlertStructureOffline,
	},
	AlertStructureOwnershipGroup: {AlertStructureOwnership},
	AlertStructureFuel: {
		AlertStructureFuelAlert, AlertStructureLowReagents, AlertStructureNoReagents,
	},
	AlertStructureIntegrity:   {AlertStructureLostShields, AlertStructureLostArmor},
	AlertStructureAbandonment: {AlertStructureImpendingAbandonment},
	AlertSovereignty: {
		AlertEntosisCaptureStarted, AlertEntosisCaptureFinished, AlertEntosisCaptureNodesReinforced,
		AlertSovStationEnteredReinforce, AlertSovStationExitedReinforce, AlertSovStructureReinforced,
		AlertSovStructureDestroyed, AlertSovCommandNodeEventStarted, AlertSovAllClaimAcquiredMsg,
		AlertSovAllClaimLostMsg, AlertSovStructureSelfDestructRequested, AlertSovStructureSelfDestructCancel,
		AlertSovStructureSelfDestructFinished, AlertSovereigntyClaimed, AlertSovereigntyLost,
	},
	AlertSovereigntyEntosis: {AlertEntosisCaptureStarted, AlertEntosisCaptureFinished, AlertEntosisCaptureNodesReinforced},
	AlertSovereigntyEvents: {
		AlertSovStationEnteredReinforce, AlertSovStationExitedReinforce, AlertSovStructureReinforced,
		AlertSovStructureDestroyed, AlertSovCommandNodeEventStarted, AlertSovAllClaimAcquiredMsg,
		AlertSovAllClaimLostMsg, AlertSovStructureSelfDestructRequested, AlertSovStructureSelfDestructCancel,
		AlertSovStructureSelfDestructFinished, AlertSovereigntyClaimed, AlertSovereigntyLost,
	},
	AlertESS:             {AlertESSMainBankLink, AlertESSReserveBankLink},
	AlertSkyhook:         {AlertSkyhookUnderAttack, AlertSkyhookLostShields, AlertSkyhookDestroyed, AlertSkyhookOnline, AlertSkyhookDeployed},
	AlertMercenaryDen:    {AlertMercenaryDenReinforced, AlertMercenaryDenAttacked, AlertMercenaryDenNewMTO},
	AlertMoonmining:      {AlertMoonminingExtractionStarted, AlertMoonminingExtractionCancelled, AlertMoonminingExtractionFinished, AlertMoonminingLaserFired, AlertMoonminingAutomaticFracture},
	AlertStationServices: {AlertStationServiceEnabled, AlertStationServiceDisabled},
	AlertStarbase:        {AlertStarbaseUnderAttack, AlertStarbaseResourceAlert},
	AlertCustomsOffices:  {AlertCustomsOfficeAttacked, AlertCustomsOfficeReinforced},
}

var alertTypes = map[string]alertDefinition{
	"StructureUnderAttack":                      {group: AlertStructureCombat, leaf: AlertStructureUnderAttack},
	"StructureDestroyed":                        {group: AlertStructureCombat, leaf: AlertStructureDestroyed},
	"StructureAnchoring":                        {group: AlertStructureState, leaf: AlertStructureAnchoring},
	"StructureUnanchoring":                      {group: AlertStructureState, leaf: AlertStructureUnanchoring},
	"StructureVulnerable":                       {group: AlertStructureState, leaf: AlertStructureVulnerable},
	"StructureReinforced":                       {group: AlertStructureState, leaf: AlertStructureReinforced},
	"StructureOnline":                           {group: AlertStructureState, leaf: AlertStructureOnline},
	"StructureWentLowPower":                     {group: AlertStructureState, leaf: AlertStructureWentLowPower},
	"StructureWentHighPower":                    {group: AlertStructureState, leaf: AlertStructureWentHighPower},
	"StructuresReinforcementChanged":            {group: AlertStructureState, leaf: AlertStructuresReinforcementChanged},
	"StructureServicesOffline":                  {group: AlertStructureState, leaf: AlertStructureOffline},
	"StructureFuelAlert":                        {group: AlertStructureFuel, leaf: AlertStructureFuelAlert},
	"StructureLowReagentsAlert":                 {group: AlertStructureFuel, leaf: AlertStructureLowReagents},
	"StructureNoReagentsAlert":                  {group: AlertStructureFuel, leaf: AlertStructureNoReagents},
	"StructureLostShields":                      {group: AlertStructureIntegrity, leaf: AlertStructureLostShields},
	"StructureLostArmor":                        {group: AlertStructureIntegrity, leaf: AlertStructureLostArmor},
	"StructureImpendingAbandonmentAssetsAtRisk": {group: AlertStructureAbandonment, leaf: AlertStructureImpendingAbandonment},
	"EntosisCaptureStarted":                     {group: AlertSovereigntyEntosis, leaf: AlertEntosisCaptureStarted},
	"EntosisCaptureFinished":                    {group: AlertSovereigntyEntosis, leaf: AlertEntosisCaptureFinished},
	"EntosisCaptureNodesReinforced":             {group: AlertSovereigntyEntosis, leaf: AlertEntosisCaptureNodesReinforced},
	"ESSMainBankLink":                           {group: AlertESS, leaf: AlertESSMainBankLink},
	"ESSReserveBankLink":                        {group: AlertESS, leaf: AlertESSReserveBankLink},
	"SkyhookUnderAttack":                        {group: AlertSkyhook, leaf: AlertSkyhookUnderAttack},
	"SkyhookLostShields":                        {group: AlertSkyhook, leaf: AlertSkyhookLostShields},
	"SkyhookDestroyed":                          {group: AlertSkyhook, leaf: AlertSkyhookDestroyed},
	"SkyhookOnline":                             {group: AlertSkyhook, leaf: AlertSkyhookOnline},
	"SkyhookDeployed":                           {group: AlertSkyhook, leaf: AlertSkyhookDeployed},
	"MercenaryDenReinforced":                    {group: AlertMercenaryDen, leaf: AlertMercenaryDenReinforced},
	"MercenaryDenAttacked":                      {group: AlertMercenaryDen, leaf: AlertMercenaryDenAttacked},
	"MercenaryDenNewMTO":                        {group: AlertMercenaryDen, leaf: AlertMercenaryDenNewMTO},
	"MoonminingExtractionStarted":               {group: AlertMoonmining, leaf: AlertMoonminingExtractionStarted},
	"MoonminingExtractionCancelled":             {group: AlertMoonmining, leaf: AlertMoonminingExtractionCancelled},
	"MoonminingExtractionFinished":              {group: AlertMoonmining, leaf: AlertMoonminingExtractionFinished},
	"MoonminingLaserFired":                      {group: AlertMoonmining, leaf: AlertMoonminingLaserFired},
	"MoonminingAutomaticFracture":               {group: AlertMoonmining, leaf: AlertMoonminingAutomaticFracture},
	"OwnershipTransferred":                      {group: AlertStructureOwnershipGroup, leaf: AlertStructureOwnership},
	"SovCommandNodeEventStarted":                {group: AlertSovereigntyEvents, leaf: AlertSovCommandNodeEventStarted},
	"SovStationEnteredReinforce":                {group: AlertSovereigntyEvents, leaf: AlertSovStationEnteredReinforce},
	"SovStationExitedReinforce":                 {group: AlertSovereigntyEvents, leaf: AlertSovStationExitedReinforce},
	"SovStructureReinforced":                    {group: AlertSovereigntyEvents, leaf: AlertSovStructureReinforced},
	"SovStructureDestroyed":                     {group: AlertSovereigntyEvents, leaf: AlertSovStructureDestroyed},
	"SovStructureSelfDestructRequested":         {group: AlertSovereigntyEvents, leaf: AlertSovStructureSelfDestructRequested},
	"SovStructureSelfDestructCancel":            {group: AlertSovereigntyEvents, leaf: AlertSovStructureSelfDestructCancel},
	"SovStructureSelfDestructFinished":          {group: AlertSovereigntyEvents, leaf: AlertSovStructureSelfDestructFinished},
	"SovAllClaimAquiredMsg":                     {group: AlertSovereigntyEvents, leaf: AlertSovAllClaimAcquiredMsg},
	"SovAllClaimLostMsg":                        {group: AlertSovereigntyEvents, leaf: AlertSovAllClaimLostMsg},
	"SovereigntyClaimed":                        {group: AlertSovereigntyEvents, leaf: AlertSovereigntyClaimed},
	"SovereigntyLost":                           {group: AlertSovereigntyEvents, leaf: AlertSovereigntyLost},
	"StationServiceEnabled":                     {group: AlertStationServices, leaf: AlertStationServiceEnabled},
	"StationServiceDisabled":                    {group: AlertStationServices, leaf: AlertStationServiceDisabled},
	"TowerAlertMsg":                             {group: AlertStarbase, leaf: AlertStarbaseUnderAttack},
	"TowerResourceAlertMsg":                     {group: AlertStarbase, leaf: AlertStarbaseResourceAlert},
	"OrbitalAttacked":                           {group: AlertCustomsOffices, leaf: AlertCustomsOfficeAttacked},
	"OrbitalReinforced":                         {group: AlertCustomsOffices, leaf: AlertCustomsOfficeReinforced},
}

// IsAlertCategory reports whether category is a supported canonical group or leaf path.
func IsAlertCategory(category string) bool {
	return IsAlertGroup(category) || IsAlertType(category)
}

// IsAlertGroup reports whether category is a supported canonical group path.
func IsAlertGroup(category string) bool {
	_, ok := alertGroups[category]
	return ok
}

// IsAlertType reports whether value identifies an individual canonical leaf path.
func IsAlertType(value string) bool {
	for _, types := range alertGroups {
		if slices.Contains(types, value) {
			return true
		}
	}
	return false
}

// NormalizeAlertSelector validates and canonicalizes a configured group or leaf path.
func NormalizeAlertSelector(selector string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(selector))
	if value == "" {
		return "", invalidAlertSelector(selector)
	}
	valueWithoutWildcard, wildcard := strings.CutSuffix(value, ".*")
	if wildcard {
		value = valueWithoutWildcard
	}
	if strings.Contains(value, "*") {
		return "", invalidAlertSelector(selector)
	}
	for segment := range strings.SplitSeq(value, ".") {
		if !selectorSegmentPattern.MatchString(segment) {
			return "", invalidAlertSelector(selector)
		}
	}
	if IsAlertCategory(value) {
		if wildcard && !IsAlertGroup(value) {
			return "", invalidAlertSelector(selector)
		}
		return value, nil
	}
	return "", unknownAlertSelector(selector)
}

// ExpandAlertSelector resolves a canonical group or leaf path to leaf paths.
func ExpandAlertSelector(selector string) ([]string, error) {
	value, err := NormalizeAlertSelector(selector)
	if err != nil {
		return nil, err
	}
	if types, ok := alertGroups[value]; ok {
		return append([]string(nil), types...), nil
	}
	return []string{value}, nil
}

// AllAlertTypes returns every supported canonical leaf path in stable order.
func AllAlertTypes() []string {
	seen := make(map[string]struct{})
	for _, definition := range alertTypes {
		seen[definition.leaf] = struct{}{}
	}
	types := make([]string, 0, len(seen))
	for alertType := range seen {
		types = append(types, alertType)
	}
	sort.Strings(types)
	return types
}

// AlertTypesForCategory expands a canonical group or returns the supplied leaf path.
func AlertTypesForCategory(category string) []string {
	types, err := ExpandAlertSelector(category)
	if err != nil {
		return nil
	}
	return types
}

// AlertTypeInGroup reports whether alertType belongs to a canonical group or equals a leaf path.
func AlertTypeInGroup(alertType, group string) bool {
	types, err := ExpandAlertSelector(group)
	if err != nil {
		return false
	}
	return slices.Contains(types, alertType)
}

// Classify maps a known ESI notification to an alert event.
func Classify(notification *esi.Notification) (Event, bool) {
	if notification == nil {
		return Event{}, false
	}
	definition, ok := alertTypes[notification.Type]
	if !ok {
		return Event{}, false
	}
	text := PlainText(notification.Text)
	metadata := extractMetadata(text)
	structureIDs := uniqueMatches(structureIDPattern, text)
	if notification.Type == "StructuresReinforcementChanged" {
		structureIDs = uniqueMatchesLimit(bulkStructureIDPattern, metadata["allstructureinfo"], maxBulkStructureIDs)
	}
	if structureID := structureIDFromMetadata(metadata["structureid"]); structureID != "" {
		structureIDs = []string{structureID}
	}
	if len(structureIDs) == 0 && usesItemIDStructureFallback(notification.Type) {
		if structureID := structureIDFromMetadata(metadata["itemid"]); structureID != "" {
			structureIDs = []string{structureID}
		}
	}
	systemID := firstMatch(systemIDPattern, text)
	structureTypeID := firstNumber(metadata["structuretypeid"])
	if structureTypeID == "" {
		structureTypeID = firstNumber(metadata["structureshowinfodata"])
	}
	if structureTypeID == "" {
		structureTypeID = firstNumber(metadata["typeid"])
	}
	ownerCorporationID, ownerCorporationName := ownerMetadata(notification.Type, metadata)
	attackerCharacterID, attackerCharacterName, attackerCorporationID, attackerCorporationName := attackerMetadata(notification.Type, metadata)
	attackerAllianceID := firstNumber(metadata["aggressorallianceid"])
	attackerAllianceName := strings.TrimSpace(metadata["aggressoralliancename"])
	allianceName := strings.TrimSpace(metadata["alliancename"])
	actorCharacterID, actorCharacterName := actorMetadata(notification.Type, metadata)
	return Event{
		NotificationID:           notification.ID,
		NotificationType:         notification.Type,
		SenderID:                 notification.SenderID,
		SenderType:               notification.SenderType,
		Timestamp:                notification.Timestamp,
		AlertType:                definition.leaf,
		StructureIDs:             structureIDs,
		SystemID:                 systemID,
		AllianceID:               firstNumber(metadata["allianceid"]),
		AllianceName:             allianceName,
		AttackerAllianceID:       attackerAllianceID,
		AttackerCharacterID:      attackerCharacterID,
		AttackerCharacterName:    attackerCharacterName,
		AttackerAllianceName:     attackerAllianceName,
		AttackerCorporationID:    attackerCorporationID,
		AttackerCorporationName:  attackerCorporationName,
		OwnerCorporationID:       ownerCorporationID,
		OwnerCorporationName:     ownerCorporationName,
		NewOwnerCorporationID:    firstNumber(metadata["newownercorpid"]),
		NewOwnerCorporationName:  strings.TrimSpace(metadata["newownercorpname"]),
		OldOwnerCorporationID:    firstNumber(metadata["oldownercorpid"]),
		OldOwnerCorporationName:  strings.TrimSpace(metadata["oldownercorpname"]),
		StructureName:            strings.TrimSpace(metadata["structurename"]),
		PlanetID:                 firstNumber(metadata["planetid"]),
		MoonID:                   firstNumber(metadata["moonid"]),
		StructureTypeID:          structureTypeID,
		TimeLeft:                 parseEVEDuration(metadata["timeleft"]),
		VulnerableFor:            parseEVEDuration(metadata["vulnerabletime"]),
		DecloakAt:                parseEVETimestamp(metadata["decloaktime"]),
		ReinforcedUntil:          orbitalReinforcementTimestamp(notification.Type, metadata),
		ReadyAt:                  parseEVETimestamp(metadata["readytime"]),
		AutoAt:                   parseEVETimestamp(metadata["autotime"]),
		DestructAt:               parseEVETimestamp(metadata["destructtime"]),
		AbandonAfterDays:         parseInt(metadata["daysuntilabandon"]),
		ActorCharacterID:         actorCharacterID,
		ActorCharacterName:       actorCharacterName,
		ReinforcementHour:        parseIntPointer(metadata["hour"]),
		ReinforcementWeekday:     parseIntPointer(metadata["weekday"]),
		ReinforcedStructureCount: parseIntPointer(metadata["numstructures"]),
		ShieldPercentage:         parseShieldPercentage(notification.Type, metadata),
		ArmorPercentage:          parseFloatPointer(metadata["armorpercentage"]),
		HullPercentage:           parseFloatPointer(metadata["hullpercentage"]),
		ShieldValue:              parseFloatPointer(metadata["shieldvalue"]),
		ArmorValue:               parseFloatPointer(metadata["armorvalue"]),
		HullValue:                parseFloatPointer(metadata["hullvalue"]),
		ResourceRequirements:     parseResourceRequirements(notification.Type, text),
		IsActive:                 parseBoolPointer(metadata["isactive"]),
		Summary:                  stripMetadata(text),
		Text:                     text,
	}, true
}

// DisplayName returns the operator-facing name for an ESI notification type.
func DisplayName(notificationType string) string {
	if name, ok := displayNames[notificationType]; ok {
		return name
	}
	return notificationType
}

func isAttackNotification(notificationType string) bool {
	return notificationType == "StructureUnderAttack" || notificationType == "SkyhookUnderAttack" || notificationType == "MercenaryDenAttacked" || notificationType == "MercenaryDenReinforced" || notificationType == "TowerAlertMsg" || notificationType == "OrbitalAttacked" || notificationType == "OrbitalReinforced"
}

func usesItemIDStructureFallback(notificationType string) bool {
	return strings.HasPrefix(notificationType, "Skyhook") || strings.HasPrefix(notificationType, "MercenaryDen")
}

func isSovereigntyClaim(notificationType string) bool {
	return notificationType == "SovAllClaimAquiredMsg" || notificationType == "SovereigntyClaimed"
}

func ownerMetadata(notificationType string, metadata map[string]string) (id, name string) {
	id = lastNumber(metadata["ownercorplinkdata"])
	name = strings.TrimSpace(metadata["ownercorpname"])
	if notificationType == "TowerResourceAlertMsg" {
		if id == "" {
			id = firstNumber(metadata["corpid"])
		}
		if name == "" {
			name = strings.TrimSpace(metadata["corpname"])
		}
	}
	if id == "" && isSovereigntyClaim(notificationType) {
		id = firstNumber(metadata["corpid"])
		name = strings.TrimSpace(metadata["corpname"])
	}
	return id, name
}

func attackerMetadata(notificationType string, metadata map[string]string) (characterID, characterName, corporationID, corporationName string) {
	if !isAttackNotification(notificationType) {
		return "", "", "", ""
	}
	if strings.HasPrefix(notificationType, "MercenaryDen") {
		return firstNumber(metadata["aggressorcharacterid"]), "", "", strings.TrimSpace(metadata["aggressorcorporationname"])
	}
	if notificationType == "TowerAlertMsg" {
		return firstNumber(metadata["aggressorid"]), "", firstNumber(metadata["aggressorcorpid"]), ""
	}
	if notificationType == "OrbitalAttacked" || notificationType == "OrbitalReinforced" {
		return firstNumber(metadata["aggressorid"]), "", firstNumber(metadata["aggressorcorpid"]), ""
	}
	characterID = firstNumber(metadata["charid"])
	if characterID == "" {
		characterID = firstNumber(metadata["characterid"])
	}
	characterName = strings.TrimSpace(metadata["charname"])
	if characterName == "" {
		characterName = strings.TrimSpace(metadata["charactername"])
	}
	corporationID = lastNumber(metadata["corplinkdata"])
	if corporationID == "" {
		corporationID = firstNumber(metadata["corpid"])
	}
	corporationName = strings.TrimSpace(metadata["corpname"])
	return characterID, characterName, corporationID, corporationName
}

func orbitalReinforcementTimestamp(notificationType string, metadata map[string]string) time.Time {
	if notificationType == "OrbitalReinforced" {
		if timestamp := parseEVETimestamp(metadata["reinforceexittime"]); !timestamp.IsZero() {
			return timestamp
		}
	}
	return parseEVETimestamp(metadata["timestampexited"])
}

func parseShieldPercentage(notificationType string, metadata map[string]string) *float64 {
	if value := parseFloatPointer(metadata["shieldpercentage"]); value != nil {
		return value
	}
	if notificationType != "OrbitalAttacked" {
		return nil
	}
	value := parseFloatPointer(metadata["shieldlevel"])
	if value == nil {
		return nil
	}
	percentage := *value
	if percentage >= 0 && percentage <= 1 {
		percentage *= 100
	}
	return &percentage
}

func parseResourceRequirements(notificationType, text string) []ResourceRequirement {
	if notificationType != "TowerResourceAlertMsg" {
		return nil
	}
	matches := resourceRequirementPattern.FindAllStringSubmatch(text, maxResourceRequirements)
	if len(matches) == 0 {
		return nil
	}
	requirements := make([]ResourceRequirement, 0, len(matches))
	for _, match := range matches {
		if len(match) < regexSubmatchCount+1 {
			continue
		}
		quantity, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil || quantity <= 0 {
			continue
		}
		requirements = append(requirements, ResourceRequirement{Quantity: quantity, TypeID: match[2]})
	}
	return requirements
}

func actorMetadata(notificationType string, metadata map[string]string) (characterID, characterName string) {
	switch notificationType {
	case "SovStructureSelfDestructRequested", "SovStructureSelfDestructCancel":
		characterID = firstNumber(metadata["charid"])
	case "MoonminingExtractionStarted":
		characterID = firstNumber(metadata["startedby"])
	case "MoonminingExtractionCancelled":
		characterID = firstNumber(metadata["cancelledby"])
	case "MoonminingLaserFired":
		characterID = firstNumber(metadata["firedby"])
	}
	characterName = strings.TrimSpace(metadata["charname"])
	if characterName == "" {
		characterName = strings.TrimSpace(metadata["charactername"])
	}
	return characterID, characterName
}

// PlainText converts ESI notification markup into bounded human-readable text.
func PlainText(value string) string {
	value = html.UnescapeString(value)
	value = tagPattern.ReplaceAllString(value, " ")
	return strings.TrimSpace(spacePattern.ReplaceAllString(value, " "))
}

func uniqueMatches(pattern *regexp.Regexp, value string) []string {
	return uniqueMatchesLimit(pattern, value, maxStructureIDs)
}

func uniqueMatchesLimit(pattern *regexp.Regexp, value string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	matches := pattern.FindAllStringSubmatch(value, limit)
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, min(len(matches), limit))
	for _, match := range matches {
		if len(match) < regexSubmatchCount {
			continue
		}
		if _, ok := seen[match[1]]; ok {
			continue
		}
		seen[match[1]] = struct{}{}
		out = append(out, match[1])
		if len(out) == limit {
			break
		}
	}
	return out
}

func firstMatch(pattern *regexp.Regexp, value string) string {
	match := pattern.FindStringSubmatch(value)
	if len(match) < regexSubmatchCount {
		return ""
	}
	return match[1]
}

func extractMetadata(value string) map[string]string {
	matches := metadataKeyPattern.FindAllStringIndex(value, -1)
	metadata := make(map[string]string, len(matches))
	for i, match := range matches {
		keyText := value[match[0]:match[1]]
		key := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(keyText, ":")))
		valueStart := match[1]
		valueEnd := len(value)
		if i+1 < len(matches) {
			valueEnd = matches[i+1][0]
		}
		metadata[key] = strings.TrimSpace(value[valueStart:valueEnd])
	}
	return metadata
}

func stripMetadata(value string) string {
	matches := metadataKeyPattern.FindAllStringIndex(value, -1)
	if len(matches) == 0 {
		return value
	}
	return strings.TrimSpace(value[:matches[0][0]])
}

func firstNumber(value string) string {
	return numberPattern.FindString(value)
}

func structureIDFromMetadata(value string) string {
	numbers := numberPattern.FindAllString(value, -1)
	if len(numbers) == 0 {
		return ""
	}
	if strings.Contains(strings.ToLower(value), "&id") {
		return numbers[len(numbers)-1]
	}
	return numbers[0]
}

func lastNumber(value string) string {
	matches := numberPattern.FindAllString(value, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1]
}

func parseInt(value string) int {
	parsed, err := strconv.Atoi(firstNumber(value))
	if err != nil || parsed <= 0 {
		return 0
	}
	return parsed
}

func parseIntPointer(value string) *int {
	parsed, err := strconv.Atoi(firstNumber(value))
	if err != nil || parsed < 0 {
		return nil
	}
	return &parsed
}

func parseEVEDuration(value string) time.Duration {
	ticks, err := strconv.ParseInt(firstNumber(value), 10, 64)
	if err != nil || ticks <= 0 || ticks > maxEVEIntervalTicks {
		return 0
	}
	return time.Duration(ticks) * eveTickDuration
}

func parseEVETimestamp(value string) time.Time {
	ticks, err := strconv.ParseInt(firstNumber(value), 10, 64)
	if err != nil || ticks <= 0 {
		return time.Time{}
	}
	seconds := ticks / eveTicksPerSecond
	nanoseconds := (ticks % eveTicksPerSecond) * eveNanosecondsPerTick
	return time.Unix(seconds-eveEpochOffsetSeconds, nanoseconds).UTC()
}

func parseFloatPointer(value string) *float64 {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return nil
	}
	return &parsed
}

func parseBoolPointer(value string) *bool {
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return nil
	}
	return &parsed
}
