// Copyright 2014 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)
//
// Functions for controlling the arena and match play.

package field

import (
	"fmt"
	"log"
	"reflect"
	"time"

	"github.com/Team254/cheesy-arena/game"
	"github.com/Team254/cheesy-arena/model"
	"github.com/Team254/cheesy-arena/network"
	"github.com/Team254/cheesy-arena/partner"
	"github.com/Team254/cheesy-arena/playoff"
	"github.com/Team254/cheesy-arena/plc"
)

const (
	arenaLoopPeriodMs        = 10
	dsPacketPeriodMs         = 250
	periodicTaskPeriodSec    = 30
	matchEndScoreDwellSec    = 3
	postTimeoutSec           = 4
	preLoadNextMatchDelaySec = 5
	scheduledBreakDelaySec   = 5
	earlyLateThresholdMin    = 2.5
	MaxMatchGapMin           = 20
)

// Progression of match states.
type MatchState int

const (
	PreMatch MatchState = iota
	StartMatch
	WarmupPeriod
	AutoPeriod
	PausePeriod
	TeleopPeriod
	PostMatch
	TimeoutActive
	PostTimeout
)

type Arena struct {
	Database         *model.Database
	EventSettings    *model.EventSettings
	accessPoint      network.AccessPoint
	networkSwitch    *network.Switch
	Plc              plc.Plc
	TbaClient        *partner.TbaClient
	NexusClient      *partner.NexusClient
	AllianceStations map[string]*AllianceStation
	Displays         map[string]*Display
	TeamSigns        *TeamSigns
	ScoringPanelRegistry
	ArenaNotifiers
	MatchState
	lastMatchState                    MatchState
	CurrentMatch                      *model.Match
	MatchStartTime                    time.Time
	LastMatchTimeSec                  float64
	RedRealtimeScore                  *RealtimeScore
	BlueRealtimeScore                 *RealtimeScore
	lastDsPacketTime                  time.Time
	lastPeriodicTaskTime              time.Time
	EventStatus                       EventStatus
	FieldReset                        bool
	AudienceDisplayMode               string
	SavedMatch                        *model.Match
	SavedMatchResult                  *model.MatchResult
	SavedRankings                     game.Rankings
	AllianceStationDisplayMode        string
	AllianceSelectionAlliances        []model.Alliance
	AllianceSelectionShowTimer        bool
	AllianceSelectionTimeRemainingSec int
	PlayoffTournament                 *playoff.PlayoffTournament
	LowerThird                        *model.LowerThird
	ShowLowerThird                    bool
	MuteMatchSounds                   bool
	matchAborted                      bool
	soundsPlayed                      map[*game.MatchSound]struct{}
	breakDescription                  string
	preloadedTeams                    *[6]*model.Team
}

type AllianceStation struct {
	DsConn     *DriverStationConnection
	Ethernet   bool
	AStop      bool
	EStop      bool
	Bypass     bool
	Team       *model.Team
	WifiStatus network.TeamWifiStatus
	aStopReset bool
}

// Creates the arena and sets it to its initial state.
func NewArena(dbPath string) (*Arena, error) {
	arena := new(Arena)
	arena.configureNotifiers()
	arena.Plc = new(plc.ModbusPlc)

	arena.AllianceStations = make(map[string]*AllianceStation)
	arena.AllianceStations["R1"] = new(AllianceStation)
	arena.AllianceStations["R2"] = new(AllianceStation)
	arena.AllianceStations["R3"] = new(AllianceStation)
	arena.AllianceStations["B1"] = new(AllianceStation)
	arena.AllianceStations["B2"] = new(AllianceStation)
	arena.AllianceStations["B3"] = new(AllianceStation)

	arena.Displays = make(map[string]*Display)

	arena.TeamSigns = NewTeamSigns()

	var err error
	arena.Database, err = model.OpenDatabase(dbPath)
	if err != nil {
		return nil, err
	}
	err = arena.LoadSettings()
	if err != nil {
		return nil, err
	}

	arena.ScoringPanelRegistry.initialize()

	// Load empty match as current.
	arena.MatchState = PreMatch
	arena.LoadTestMatch()
	arena.LastMatchTimeSec = 0
	arena.lastMatchState = -1

	// Initialize display parameters.
	arena.AudienceDisplayMode = "blank"
	arena.SavedMatch = &model.Match{}
	arena.SavedMatchResult = model.NewMatchResult()
	arena.AllianceStationDisplayMode = "match"

	return arena, nil
}

// Loads or reloads the event settings upon initial setup or change.
func (arena *Arena) LoadSettings() error {
	settings, err := arena.Database.GetEventSettings()
	if err != nil {
		return err
	}
	arena.EventSettings = settings

	// Initialize the components that depend on settings.
	arena.TeamSigns.Red1.SetAddress(settings.TeamSignRed1Address)
	arena.TeamSigns.Red2.SetAddress(settings.TeamSignRed2Address)
	arena.TeamSigns.Red3.SetAddress(settings.TeamSignRed3Address)
	arena.TeamSigns.RedTimer.SetAddress(settings.TeamSignRedTimerAddress)
	arena.TeamSigns.Blue1.SetAddress(settings.TeamSignBlue1Address)
	arena.TeamSigns.Blue2.SetAddress(settings.TeamSignBlue2Address)
	arena.TeamSigns.Blue3.SetAddress(settings.TeamSignBlue3Address)
	arena.TeamSigns.BlueTimer.SetAddress(settings.TeamSignBlueTimerAddress)
	accessPointWifiStatuses := [6]*network.TeamWifiStatus{
		&arena.AllianceStations["R1"].WifiStatus,
		&arena.AllianceStations["R2"].WifiStatus,
		&arena.AllianceStations["R3"].WifiStatus,
		&arena.AllianceStations["B1"].WifiStatus,
		&arena.AllianceStations["B2"].WifiStatus,
		&arena.AllianceStations["B3"].WifiStatus,
	}
	arena.accessPoint.SetSettings(
		settings.ApAddress,
		settings.ApPassword,
		settings.ApChannel,
		settings.NetworkSecurityEnabled,
		accessPointWifiStatuses,
	)
	arena.networkSwitch = network.NewSwitch(settings.SwitchAddress, settings.SwitchPassword)
	arena.Plc.SetAddress(settings.PlcAddress)
	arena.TbaClient = partner.NewTbaClient(settings.TbaEventCode, settings.TbaSecretId, settings.TbaSecret)
	arena.NexusClient = partner.NewNexusClient(settings.TbaEventCode)

	game.MatchTiming.WarmupDurationSec = settings.WarmupDurationSec
	game.MatchTiming.AutoDurationSec = settings.AutoDurationSec
	game.MatchTiming.PauseDurationSec = settings.PauseDurationSec
	game.MatchTiming.TeleopDurationSec = settings.TeleopDurationSec
	game.MatchTiming.WarningRemainingDurationSec = settings.WarningRemainingDurationSec
	game.UpdateMatchSounds()
	arena.MatchTimingNotifier.Notify()

	game.MelodyBonusThresholdWithoutCoop = settings.MelodyBonusThresholdWithoutCoop
	game.MelodyBonusThresholdWithCoop = settings.MelodyBonusThresholdWithCoop
	game.AmplificationNoteLimit = settings.AmplificationNoteLimit
	game.AmplificationDurationSec = settings.AmplificationDurationSec

	// Reconstruct the playoff tournament in memory.
	if err = arena.CreatePlayoffTournament(); err != nil {
		return err
	}
	if err = arena.UpdatePlayoffTournament(); err != nil {
		return err
	}

	return nil
}

// Constructs an empty playoff tournament in memory, based only on the number of alliances.
func (arena *Arena) CreatePlayoffTournament() error {
	var err error
	arena.PlayoffTournament, err = playoff.NewPlayoffTournament(
		arena.EventSettings.PlayoffType, arena.EventSettings.NumPlayoffAlliances,
	)
	return err
}

// Performs the one-time creation of all matches for the playoff tournament.
func (arena *Arena) CreatePlayoffMatches(startTime time.Time) error {
	return arena.PlayoffTournament.CreateMatchesAndBreaks(arena.Database, startTime)
}

// Traverses the playoff tournament rounds to assess winners and populate subsequent matches.
func (arena *Arena) UpdatePlayoffTournament() error {
	alliances, err := arena.Database.GetAllAlliances()
	if err != nil {
		return err
	}
	if len(alliances) > 0 {
		return arena.PlayoffTournament.UpdateMatches(arena.Database)
	}
	return nil
}

// Sets up the arena for the given match.
func (arena *Arena) LoadMatch(match *model.Match) error {
	if arena.MatchState != PreMatch {
		return fmt.Errorf("cannot load match while there is a match still in progress or with results pending")
	}

	arena.CurrentMatch = match

	loadedByNexus := false
	if match.ShouldAllowNexusSubstitution() && arena.EventSettings.NexusEnabled {
		// Attempt to get the match lineup from Nexus for FRC.
		lineup, err := arena.NexusClient.GetLineup(match.TbaMatchKey)
		if err != nil {
			log.Printf("Failed to load lineup from Nexus: %s", err.Error())
		} else {
			err = arena.SubstituteTeams(lineup[0], lineup[1], lineup[2], lineup[3], lineup[4], lineup[5])
			if err != nil {
				log.Printf("Failed to substitute teams using Nexus lineup; loading match normally: %s", err.Error())
			} else {
				log.Printf(
					"Successfully loaded lineup for match %s from Nexus: %v", match.TbaMatchKey.String(), *lineup,
				)
				loadedByNexus = true
			}
		}
	}

	if !loadedByNexus {
		err := arena.assignTeam(match.Red1, "R1")
		if err != nil {
			return err
		}
		err = arena.assignTeam(match.Red2, "R2")
		if err != nil {
			return err
		}
		err = arena.assignTeam(match.Red3, "R3")
		if err != nil {
			return err
		}
		err = arena.assignTeam(match.Blue1, "B1")
		if err != nil {
			return err
		}
		err = arena.assignTeam(match.Blue2, "B2")
		if err != nil {
			return err
		}
		err = arena.assignTeam(match.Blue3, "B3")
		if err != nil {
			return err
		}

		arena.setupNetwork(
			[6]*model.Team{
				arena.AllianceStations["R1"].Team,
				arena.AllianceStations["R2"].Team,
				arena.AllianceStations["R3"].Team,
				arena.AllianceStations["B1"].Team,
				arena.AllianceStations["B2"].Team,
				arena.AllianceStations["B3"].Team,
			},
			false,
		)
	}

	// Reset the arena state and realtime scores.
	arena.soundsPlayed = make(map[*game.MatchSound]struct{})
	arena.RedRealtimeScore = NewRealtimeScore()
	arena.BlueRealtimeScore = NewRealtimeScore()
	arena.ScoringPanelRegistry.resetScoreCommitted()
	arena.Plc.ResetMatch()

	// Notify any listeners about the new match.
	arena.MatchLoadNotifier.Notify()
	arena.RealtimeScoreNotifier.Notify()
	arena.AllianceStationDisplayMode = "match"
	arena.AllianceStationDisplayModeNotifier.Notify()
	arena.ScoringStatusNotifier.Notify()

	return nil
}

// Sets a new test match containing no teams as the current match.
func (arena *Arena) LoadTestMatch() error {
	return arena.LoadMatch(&model.Match{Type: model.Test, ShortName: "T", LongName: "Test Match"})
}

// Loads the first unplayed match of the current match type.
func (arena *Arena) LoadNextMatch(startScheduledBreak bool) error {
	nextMatch, err := arena.getNextMatch(false)
	if err != nil {
		return err
	}
	if nextMatch == nil {
		return arena.LoadTestMatch()
	}
	err = arena.LoadMatch(nextMatch)
	if err != nil {
		return err
	}

	// Start the timeout timer if there is a scheduled break before this match.
	if startScheduledBreak {
		scheduledBreak, err := arena.Database.GetScheduledBreakByMatchTypeOrder(nextMatch.Type, nextMatch.TypeOrder)
		if err != nil {
			return err
		}
		if scheduledBreak != nil {
			go func() {
				time.Sleep(time.Second * scheduledBreakDelaySec)
				_ = arena.StartTimeout(scheduledBreak.Description, scheduledBreak.DurationSec)
			}()
		}
	}

	return nil
}

// Assigns the given team to the given station, also substituting it into the match record.
func (arena *Arena) SubstituteTeams(red1, red2, red3, blue1, blue2, blue3 int) error {
	if !arena.CurrentMatch.ShouldAllowSubstitution() {
		return fmt.Errorf("Can't substitute teams for qualification matches.")
	}

	if err := arena.validateTeams(red1, red2, red3, blue1, blue2, blue3); err != nil {
		return err
	}
	if err := arena.assignTeam(red1, "R1"); err != nil {
		return err
	}
	if err := arena.assignTeam(red2, "R2"); err != nil {
		return err
	}
	if err := arena.assignTeam(red3, "R3"); err != nil {
		return err
	}
	if err := arena.assignTeam(blue1, "B1"); err != nil {
		return err
	}
	if err := arena.assignTeam(blue2, "B2"); err != nil {
		return err
	}
	if err := arena.assignTeam(blue3, "B3"); err != nil {
		return err
	}

	arena.CurrentMatch.Red1 = red1
	arena.CurrentMatch.Red2 = red2
	arena.CurrentMatch.Red3 = red3
	arena.CurrentMatch.Blue1 = blue1
	arena.CurrentMatch.Blue2 = blue2
	arena.CurrentMatch.Blue3 = blue3
	arena.setupNetwork(
		[6]*model.Team{
			arena.AllianceStations["R1"].Team,
			arena.AllianceStations["R2"].Team,
			arena.AllianceStations["R3"].Team,
			arena.AllianceStations["B1"].Team,
			arena.AllianceStations["B2"].Team,
			arena.AllianceStations["B3"].Team,
		},
		false,
	)
	arena.MatchLoadNotifier.Notify()

	if arena.CurrentMatch.Type != model.Test {
		arena.Database.UpdateMatch(arena.CurrentMatch)
	}
	return nil
}

// Starts the match if all conditions are met.
func (arena *Arena) StartMatch() error {
	err := arena.checkCanStartMatch()
	if err == nil {
		// Save the match start time to the database for posterity.
		arena.CurrentMatch.StartedAt = time.Now()
		if arena.CurrentMatch.Type != model.Test {
			arena.Database.UpdateMatch(arena.CurrentMatch)
		}
		arena.updateCycleTime(arena.CurrentMatch.StartedAt)

		// Save the missed packet count to subtract it from the running count.
		for _, allianceStation := range arena.AllianceStations {
			if allianceStation.DsConn != nil {
				err = allianceStation.DsConn.signalMatchStart(arena.CurrentMatch, &allianceStation.WifiStatus)
				if err != nil {
					log.Println(err)
				}
			}

			// Save the teams that have successfully connected to the field.
			if allianceStation.Team != nil && !allianceStation.Team.HasConnected && allianceStation.DsConn != nil &&
				allianceStation.DsConn.RobotLinked {
				allianceStation.Team.HasConnected = true
				arena.Database.UpdateTeam(allianceStation.Team)
			}
		}

		arena.MatchState = StartMatch
	}
	return err
}

// Kills the current match or timeout if it is underway.
func (arena *Arena) AbortMatch() error {
	if arena.MatchState == PreMatch || arena.MatchState == PostMatch || arena.MatchState == PostTimeout {
		return fmt.Errorf("cannot abort match when it is not in progress")
	}

	if arena.MatchState == TimeoutActive {
		// Handle by advancing the timeout clock to the end and letting the regular logic deal with it.
		arena.MatchStartTime = time.Now().Add(-time.Second * time.Duration(game.MatchTiming.TimeoutDurationSec))
		return nil
	}

	if arena.MatchState != WarmupPeriod {
		arena.playSound("abort")
	}
	arena.MatchState = PostMatch
	arena.matchAborted = true
	arena.AudienceDisplayMode = "blank"
	arena.AudienceDisplayModeNotifier.Notify()
	arena.AllianceStationDisplayMode = "logo"
	arena.AllianceStationDisplayModeNotifier.Notify()
	return nil
}

// Clears out the match and resets the arena state unless there is a match underway.
func (arena *Arena) ResetMatch() error {
	if arena.MatchState != PostMatch && arena.MatchState != PreMatch {
		return fmt.Errorf("cannot reset match while it is in progress")
	}
	arena.MatchState = PreMatch
	arena.matchAborted = false
	arena.AllianceStations["R1"].Bypass = false
	arena.AllianceStations["R2"].Bypass = false
	arena.AllianceStations["R3"].Bypass = false
	arena.AllianceStations["B1"].Bypass = false
	arena.AllianceStations["B2"].Bypass = false
	arena.AllianceStations["B3"].Bypass = false
	arena.MuteMatchSounds = false
	return nil
}

// Starts a timeout of the given duration.
func (arena *Arena) StartTimeout(description string, durationSec int) error {
	if arena.MatchState != PreMatch {
		return fmt.Errorf("cannot start timeout while there is a match still in progress or with results pending")
	}

	game.MatchTiming.TimeoutDurationSec = durationSec
	game.UpdateMatchSounds()
	arena.soundsPlayed = make(map[*game.MatchSound]struct{})
	arena.MatchTimingNotifier.Notify()
	arena.breakDescription = description
	arena.MatchLoadNotifier.Notify()
	arena.MatchState = TimeoutActive
	arena.MatchStartTime = time.Now()
	arena.LastMatchTimeSec = -1
	arena.AllianceStationDisplayMode = "timeout"
	arena.AllianceStationDisplayModeNotifier.Notify()

	return nil
}

// Updates the audience display screen.
func (arena *Arena) SetAudienceDisplayMode(mode string) {
	if arena.AudienceDisplayMode != mode {
		arena.AudienceDisplayMode = mode
		arena.AudienceDisplayModeNotifier.Notify()
		if mode == "score" {
			arena.playSound("match_result")
		}
	}
}

// Updates the alliance station display screen.
func (arena *Arena) SetAllianceStationDisplayMode(mode string) {
	if arena.AllianceStationDisplayMode != mode {
		arena.AllianceStationDisplayMode = mode
		arena.AllianceStationDisplayModeNotifier.Notify()
	}
}

// Returns the fractional number of seconds since the start of the match.
func (arena *Arena) MatchTimeSec() float64 {
	if arena.MatchState == PreMatch || arena.MatchState == StartMatch || arena.MatchState == PostMatch {
		return 0
	} else {
		return time.Since(arena.MatchStartTime).Seconds()
	}
}

// Performs a single iteration of checking inputs and timers and setting outputs accordingly to control the
// flow of a match.
func (arena *Arena) Update() {
	// Decide what state the robots need to be in, depending on where we are in the match.
	auto := false
	enabled := false
	sendDsPacket := false
	matchTimeSec := arena.MatchTimeSec()
	switch arena.MatchState {
	case PreMatch:
		auto = true
		enabled = false
	case StartMatch:
		arena.MatchStartTime = time.Now()
		arena.LastMatchTimeSec = -1
		auto = true
		arena.AudienceDisplayMode = "match"
		arena.AudienceDisplayModeNotifier.Notify()
		arena.AllianceStationDisplayMode = "match"
		arena.AllianceStationDisplayModeNotifier.Notify()
		if game.MatchTiming.WarmupDurationSec > 0 {
			arena.MatchState = WarmupPeriod
			enabled = false
			sendDsPacket = false
		} else {
			arena.MatchState = AutoPeriod
			enabled = true
			sendDsPacket = true
		}
		arena.Plc.ResetMatch()
		arena.FieldReset = false
	case WarmupPeriod:
		auto = true
		enabled = false
		if matchTimeSec >= float64(game.MatchTiming.WarmupDurationSec) {
			arena.MatchState = AutoPeriod
			auto = true
			enabled = true
			sendDsPacket = true
		}
	case AutoPeriod:
		auto = true
		enabled = true
		if matchTimeSec >= game.GetDurationToAutoEnd().Seconds() {
			auto = false
			sendDsPacket = true
			if game.MatchTiming.PauseDurationSec > 0 {
				arena.MatchState = PausePeriod
				enabled = false
			} else {
				arena.MatchState = TeleopPeriod
				enabled = true
			}
		}
	case PausePeriod:
		auto = false
		enabled = false
		if matchTimeSec >= game.GetDurationToTeleopStart().Seconds() {
			arena.MatchState = TeleopPeriod
			auto = false
			enabled = true
			sendDsPacket = true
		}
	case TeleopPeriod:
		auto = false
		enabled = true
		if matchTimeSec >= game.GetDurationToTeleopEnd().Seconds() {
			arena.MatchState = PostMatch
			auto = false
			enabled = false
			sendDsPacket = true
			go func() {
				// Leave the scores on the screen briefly at the end of the match.
				time.Sleep(time.Second * matchEndScoreDwellSec)
				arena.AudienceDisplayMode = "blank"
				arena.AudienceDisplayModeNotifier.Notify()
				arena.AllianceStationDisplayMode = "logo"
				arena.AllianceStationDisplayModeNotifier.Notify()
			}()
			go func() {
				// Configure the network in advance for the next match after a delay.
				time.Sleep(time.Second * preLoadNextMatchDelaySec)
				arena.preLoadNextMatch()
			}()
		}
	case TimeoutActive:
		if matchTimeSec >= float64(game.MatchTiming.TimeoutDurationSec) {
			arena.MatchState = PostTimeout
			go func() {
				// Leave the timer on the screen briefly at the end of the timeout period.
				time.Sleep(time.Second * matchEndScoreDwellSec)
				arena.AudienceDisplayMode = "blank"
				arena.AudienceDisplayModeNotifier.Notify()
				arena.AllianceStationDisplayMode = "logo"
				arena.AllianceStationDisplayModeNotifier.Notify()
			}()
		}
	case PostTimeout:
		if matchTimeSec >= float64(game.MatchTiming.TimeoutDurationSec+postTimeoutSec) {
			arena.MatchState = PreMatch
		}
	}

	// Send a match tick notification if passing an integer second threshold or if the match state changed.
	if int(matchTimeSec) != int(arena.LastMatchTimeSec) || arena.MatchState != arena.lastMatchState {
		arena.MatchTimeNotifier.Notify()
	}

	// Send a packet if at a period transition point or if it's been long enough since the last one.
	if sendDsPacket || time.Since(arena.lastDsPacketTime).Seconds()*1000 >= dsPacketPeriodMs {
		arena.sendDsPacket(auto, enabled)
		arena.ArenaStatusNotifier.Notify()
	}

	arena.handleSounds(matchTimeSec)

	// Handle field sensors/lights/actuators.
	arena.handlePlcInputOutput()

	// Handle the team number / timer displays.
	arena.TeamSigns.Update(arena)

	arena.LastMatchTimeSec = matchTimeSec
	arena.lastMatchState = arena.MatchState
}

// Loops indefinitely to track and update the arena components.
func (arena *Arena) Run() {
	// Start other loops in goroutines.
	go arena.listenForDriverStations()
	go arena.listenForDsUdpPackets()
	go arena.accessPoint.Run()
	go arena.Plc.Run()

	for {
		arena.Update()
		if time.Since(arena.lastPeriodicTaskTime).Seconds() >= periodicTaskPeriodSec {
			arena.lastPeriodicTaskTime = time.Now()
			go arena.runPeriodicTasks()
		}
		time.Sleep(time.Millisecond * arenaLoopPeriodMs)
	}
}

// Calculates the red alliance score summary for the given realtime snapshot.
func (arena *Arena) RedScoreSummary() *game.ScoreSummary {
	return arena.RedRealtimeScore.CurrentScore.Summarize(&arena.BlueRealtimeScore.CurrentScore)
}

// Calculates the blue alliance score summary for the given realtime snapshot.
func (arena *Arena) BlueScoreSummary() *game.ScoreSummary {
	return arena.BlueRealtimeScore.CurrentScore.Summarize(&arena.RedRealtimeScore.CurrentScore)
}

// Checks that the given teams are present in the database, allowing team ID 0 which indicates an empty spot.
func (arena *Arena) validateTeams(teamIds ...int) error {
	for _, teamId := range teamIds {
		if teamId == 0 {
			continue
		}
		team, err := arena.Database.GetTeamById(teamId)
		if err != nil {
			return err
		}
		if team == nil {
			return fmt.Errorf("Team %d is not present at the event.", teamId)
		}
	}
	return nil
}

// Loads a team into an alliance station, cleaning up the previous team there if there is one.
func (arena *Arena) assignTeam(teamId int, station string) error {
	// Reject invalid station values.
	if _, ok := arena.AllianceStations[station]; !ok {
		return fmt.Errorf("Invalid alliance station '%s'.", station)
	}

	// Force the A-stop to be reset by the new team if it is already pressed (if the PLC is enabled).
	arena.AllianceStations[station].aStopReset = !arena.Plc.IsEnabled()

	// Do nothing if the station is already assigned to the requested team.
	dsConn := arena.AllianceStations[station].DsConn
	if dsConn != nil && dsConn.TeamId == teamId {
		return nil
	}
	if dsConn != nil {
		dsConn.close()
		arena.AllianceStations[station].Team = nil
		arena.AllianceStations[station].DsConn = nil
	}

	// Leave the station empty if the team number is zero.
	if teamId == 0 {
		arena.AllianceStations[station].Team = nil
		return nil
	}

	// Load the team model. If it doesn't exist, enable anonymous operation.
	team, err := arena.Database.GetTeamById(teamId)
	if err != nil {
		return err
	}
	if team == nil {
		team = &model.Team{Id: teamId}
	}

	arena.AllianceStations[station].Team = team
	return nil
}

// Returns the next match of the same type that is currently loaded, or nil if there are no more matches.
func (arena *Arena) getNextMatch(excludeCurrent bool) (*model.Match, error) {
	if arena.CurrentMatch.Type == model.Test {
		return nil, nil
	}

	matches, err := arena.Database.GetMatchesByType(arena.CurrentMatch.Type, false)
	if err != nil {
		return nil, err
	}
	for _, match := range matches {
		if !match.IsComplete() && !(excludeCurrent && match.Id == arena.CurrentMatch.Id) {
			return &match, nil
		}
	}

	// There are no matches left of the same type.
	return nil, nil
}

// Configures the field network for the next match in advance of the current match being scored and committed.
func (arena *Arena) preLoadNextMatch() {
	if arena.MatchState != PostMatch {
		// The next match has already been loaded; no need to do anything.
		return
	}

	nextMatch, err := arena.getNextMatch(true)
	if err != nil {
		log.Printf("Failed to pre-load next match: %s", err.Error())
	}
	if nextMatch == nil {
		return
	}

	var teams [6]*model.Team
	for i, teamId := range []int{
		nextMatch.Red1, nextMatch.Red2, nextMatch.Red3, nextMatch.Blue1, nextMatch.Blue2, nextMatch.Blue3,
	} {
		if teamId == 0 {
			continue
		}
		if teams[i], err = arena.Database.GetTeamById(teamId); err != nil {
			log.Printf("Failed to get model for Team %d while pre-loading next match: %s", teamId, err.Error())
		}
	}
	arena.setupNetwork(teams, true)
	arena.TeamSigns.SetNextMatchTeams(nextMatch)
}

// Asynchronously reconfigures the networking hardware for the new set of teams.
func (arena *Arena) setupNetwork(teams [6]*model.Team, isPreload bool) {
	if isPreload {
		arena.preloadedTeams = &teams
	} else if arena.preloadedTeams != nil {
		preloadedTeams := *arena.preloadedTeams
		arena.preloadedTeams = nil
		if reflect.DeepEqual(teams, preloadedTeams) {
			// Skip configuring the network; this is the same set of teams that was preloaded.
			return
		}
	}

	if arena.EventSettings.NetworkSecurityEnabled {
		if err := arena.accessPoint.ConfigureTeamWifi(teams); err != nil {
			log.Printf("Failed to configure team WiFi: %s", err.Error())
		}
		go func() {
			if err := arena.networkSwitch.ConfigureTeamEthernet(teams); err != nil {
				log.Printf("Failed to configure team Ethernet: %s", err.Error())
			}
		}()
	}
}

// Returns nil if the match can be started, and an error otherwise.
func (arena *Arena) checkCanStartMatch() error {
	if arena.MatchState != PreMatch {
		return fmt.Errorf("cannot start match while there is a match still in progress or with results pending")
	}

	err := arena.checkAllianceStationsReady("R1", "R2", "R3", "B1", "B2", "B3")
	if err != nil {
		return err
	}

	if arena.Plc.IsEnabled() {
		if !arena.Plc.IsHealthy() {
			return fmt.Errorf("cannot start match while PLC is not healthy")
		}
		if arena.Plc.GetFieldEStop() {
			return fmt.Errorf("cannot start match while field emergency stop is active")
		}
		for name, status := range arena.Plc.GetArmorBlockStatuses() {
			if !status {
				return fmt.Errorf("cannot start match while PLC ArmorBlock %q is not connected", name)
			}
		}
	}

	return nil
}

func (arena *Arena) checkAllianceStationsReady(stations ...string) error {
	for _, station := range stations {
		allianceStation := arena.AllianceStations[station]
		if allianceStation.EStop {
			return fmt.Errorf("cannot start match while an emergency stop is active")
		}
		if !allianceStation.aStopReset {
			return fmt.Errorf("cannot start match if an autonomous stop has not been reset since the previous match")
		}
		if !allianceStation.Bypass {
			if allianceStation.DsConn == nil || !allianceStation.DsConn.RobotLinked {
				return fmt.Errorf("cannot start match until all robots are connected or bypassed")
			}
		}
	}

	return nil
}

func (arena *Arena) sendDsPacket(auto bool, enabled bool) {
	for _, allianceStation := range arena.AllianceStations {
		dsConn := allianceStation.DsConn
		if dsConn != nil {
			dsConn.Auto = auto
			dsConn.Enabled = enabled && !allianceStation.EStop && !(auto && allianceStation.AStop) &&
				!allianceStation.Bypass
			dsConn.EStop = allianceStation.EStop
			dsConn.AStop = allianceStation.AStop
			err := dsConn.update(arena)
			if err != nil {
				log.Printf("Unable to send driver station packet for team %d.", allianceStation.Team.Id)
			}
		}
	}
	arena.lastDsPacketTime = time.Now()
}

// Returns the alliance station identifier for the given team, or the empty string if the team is not present
// in the current match.
func (arena *Arena) getAssignedAllianceStation(teamId int) string {
	for station, allianceStation := range arena.AllianceStations {
		if allianceStation.Team != nil && allianceStation.Team.Id == teamId {
			return station
		}
	}

	return ""
}
var redAmplifiedTimePostWindow_ons = false
var blueAmplifiedTimePostWindow_ons = false
// Updates the score given new input information from the field PLC, and actuates PLC outputs accordingly.
func (arena *Arena) handlePlcInputOutput() {
	if !arena.Plc.IsEnabled() {
		// Declare and initialize arrays
		redEStops, blueEStops := [3]bool{}, [3]bool{}
		redAStops, blueAStops := [3]bool{}, [3]bool{}
		// Fill arrays with false values
		for i := range redEStops {
			redEStops[i] = false
			redAStops[i] = false
		}
		for i := range blueEStops {
			blueEStops[i] = false
			blueAStops[i] = false
		}
		arena.handleTeamStop("R1", redEStops[0], redAStops[0])
		arena.handleTeamStop("R2", redEStops[1], redAStops[1])
		arena.handleTeamStop("R3", redEStops[2], redAStops[2])
		arena.handleTeamStop("B1", blueEStops[0], blueAStops[0])
		arena.handleTeamStop("B2", blueEStops[1], blueAStops[1])
		arena.handleTeamStop("B3", blueEStops[2], blueAStops[2])
		
		// Handle in-match PLC functions.
		redScore := &arena.RedRealtimeScore.CurrentScore
		oldRedScore := *redScore
		blueScore := &arena.BlueRealtimeScore.CurrentScore
		oldBlueScore := *blueScore
		matchStartTime := arena.MatchStartTime
		currentTime := time.Now()
		teleopGracePeriod := matchStartTime.Add(
			game.GetDurationToTeleopEnd() + game.SpeakerTeleopGracePeriodSec*time.Second,
		)
		inGracePeriod := arena.MatchState == PostMatch && currentTime.Before(teleopGracePeriod)

		redAllianceReady := arena.checkAllianceStationsReady("R1", "R2", "R3") == nil
		blueAllianceReady := arena.checkAllianceStationsReady("B1", "B2", "B3") == nil
		
		// Handle the evergreen PLC functions: stack lights, stack buzzer, and field reset light.
		switch arena.MatchState {
		case PreMatch:
			if arena.lastMatchState != PreMatch {
				//*arena.Plc.SetFieldResetLight(true)
			}
			fallthrough
		case TimeoutActive:
			fallthrough
		case PostTimeout:
			// Set the stack light state -- solid alliance color(s) if robots are not connected, solid orange if scores are
			// not input, or blinking green if ready.
			//*greenStackLight := redAllianceReady && blueAllianceReady //&& arena.Plc.GetCycleState(2, 0, 2)
			//*arena.Plc.SetStackLights(!redAllianceReady, !blueAllianceReady, false, greenStackLight)
			//*arena.Plc.SetStackBuzzer(redAllianceReady && blueAllianceReady)

			// Turn off lights if all teams become ready.
			if redAllianceReady && blueAllianceReady {
				arena.FieldReset = false
				//*arena.Plc.SetFieldResetLight(false)
				if arena.CurrentMatch.FieldReadyAt.IsZero() {
					arena.CurrentMatch.FieldReadyAt = time.Now()
				}
			}
		case PostMatch:
			if arena.FieldReset {
				//*arena.Plc.SetFieldResetLight(true)
			}
			//*scoreReady := arena.RedRealtimeScore.FoulsCommitted && arena.BlueRealtimeScore.FoulsCommitted &&
			//*	arena.alliancePostMatchScoreReady("red") && arena.alliancePostMatchScoreReady("blue")
			//*arena.Plc.SetStackLights(false, false, !scoreReady, false)
		case AutoPeriod, PausePeriod, TeleopPeriod:
			//*arena.Plc.SetStackBuzzer(false)
			//*arena.Plc.SetStackLights(!redAllianceReady, !blueAllianceReady, false, true)
		}

		// Get all the game-specific inputs and update the score.
		redAmplifyButton, redCoopButton, blueAmplifyButton, blueCoopButton := false, false, false, false
		redAmpNoteCount := arena.RedRealtimeScore.CurrentScore.AmpSpeaker.AutoAmpNotes +
							arena.RedRealtimeScore.CurrentScore.AmpSpeaker.TeleopAmpNotes
		redSpeakerNoteCount :=  arena.RedRealtimeScore.CurrentScore.AmpSpeaker.AutoSpeakerNotes +
								arena.RedRealtimeScore.CurrentScore.AmpSpeaker.TeleopUnamplifiedSpeakerNotes +
								arena.RedRealtimeScore.CurrentScore.AmpSpeaker.TeleopAmplifiedSpeakerNotes
		blueAmpNoteCount := arena.BlueRealtimeScore.CurrentScore.AmpSpeaker.AutoAmpNotes +
								arena.BlueRealtimeScore.CurrentScore.AmpSpeaker.TeleopAmpNotes
		blueSpeakerNoteCount :=  arena.BlueRealtimeScore.CurrentScore.AmpSpeaker.AutoSpeakerNotes +
									arena.BlueRealtimeScore.CurrentScore.AmpSpeaker.TeleopUnamplifiedSpeakerNotes +
									arena.BlueRealtimeScore.CurrentScore.AmpSpeaker.TeleopAmplifiedSpeakerNotes
		
		redAmpSpeaker := &arena.RedRealtimeScore.CurrentScore.AmpSpeaker
		blueAmpSpeaker := &arena.BlueRealtimeScore.CurrentScore.AmpSpeaker
		redAmpSpeaker.UpdateState(
			redAmpNoteCount, redSpeakerNoteCount, redAmplifyButton, redCoopButton, matchStartTime, currentTime,
		)
		blueAmpSpeaker.UpdateState(
			blueAmpNoteCount, blueSpeakerNoteCount, blueAmplifyButton, blueCoopButton, matchStartTime, currentTime,
		)
		if !oldRedScore.Equals(redScore) || !oldBlueScore.Equals(blueScore) {
			arena.RealtimeScoreNotifier.Notify()
		}

		// Handle the amp outputs.
		redAmplifiedTimeRemaining := redAmpSpeaker.AmplifiedTimeRemaining(currentTime)
		arena.RedRealtimeScore.AmplifiedTimeRemainingSec = int(redAmplifiedTimeRemaining)
		redAmplifiedTimePostWindow := redAmpSpeaker.IsAmplified(currentTime, true)
		arena.RedRealtimeScore.AmplifiedTimePostWindow = redAmplifiedTimePostWindow
		
		blueAmplifiedTimeRemaining := blueAmpSpeaker.AmplifiedTimeRemaining(currentTime)
		arena.BlueRealtimeScore.AmplifiedTimeRemainingSec = int(blueAmplifiedTimeRemaining)
		blueAmplifiedTimePostWindow := blueAmpSpeaker.IsAmplified(currentTime, true)
		arena.BlueRealtimeScore.AmplifiedTimePostWindow = blueAmplifiedTimePostWindow
		
		if arena.MatchState == AutoPeriod || arena.MatchState == PausePeriod || arena.MatchState == TeleopPeriod {
			redLowAmpLight := redAmpSpeaker.BankedAmpNotes >= 1
			redHighAmpLight := redAmpSpeaker.BankedAmpNotes >= 2
			redCoopAmpLight := redAmpSpeaker.CoopActivated
			if redAmplifiedTimeRemaining > 0 {
				redAmplifiedTimePostWindow_ons = false
				redLowAmpLight = int(redAmplifiedTimeRemaining*2)%2 == 0
				redHighAmpLight = !redLowAmpLight
				arena.RealtimeScoreNotifier.Notify()
			}
			if !redAmplifiedTimePostWindow && !redAmplifiedTimePostWindow_ons{
				arena.RealtimeScoreNotifier.Notify()
				redAmplifiedTimePostWindow_ons = true
			}
			
			blueLowAmpLight := blueAmpSpeaker.BankedAmpNotes >= 1
			blueHighAmpLight := blueAmpSpeaker.BankedAmpNotes >= 2
			blueCoopAmpLight := blueAmpSpeaker.CoopActivated
			if blueAmplifiedTimeRemaining > 0 {
				blueAmplifiedTimePostWindow_ons = false
				blueLowAmpLight = int(blueAmplifiedTimeRemaining*4)%2 == 0
				blueHighAmpLight = !blueLowAmpLight
				arena.RealtimeScoreNotifier.Notify()
			}
			if !blueAmplifiedTimePostWindow && !blueAmplifiedTimePostWindow_ons{
				arena.RealtimeScoreNotifier.Notify()
				blueAmplifiedTimePostWindow_ons = true
			}
			
			arena.Plc.SetAmpLights(
				redLowAmpLight, redHighAmpLight, redCoopAmpLight, blueLowAmpLight, blueHighAmpLight, blueCoopAmpLight,
			)
		} else if arena.MatchState == PostMatch {
			arena.Plc.SetAmpLights(false, false, false, false, false, false)
		}

		// Handle the speaker outputs.
		arena.Plc.SetSpeakerMotors(
			arena.MatchState == AutoPeriod || arena.MatchState == PausePeriod || arena.MatchState == TeleopPeriod ||
				inGracePeriod,
		)
		arena.Plc.SetSpeakerLights(
			redAmplifiedTimeRemaining > 0 && arena.MatchState != PostMatch,
			blueAmplifiedTimeRemaining > 0 && arena.MatchState != PostMatch,
		)

		// Handle the subwoofer outputs.
		arena.Plc.SetSubwooferCountdown(
			redAmplifiedTimeRemaining > 0 && arena.MatchState != PostMatch,
			blueAmplifiedTimeRemaining > 0 && arena.MatchState != PostMatch,
		)
		arena.Plc.SetPostMatchSubwooferLights(inGracePeriod)
		
		
		return
	}else{
			// Handle PLC functions that are always active.
			if arena.Plc.GetFieldEStop() && !arena.matchAborted {
				arena.AbortMatch()
			}
			redEStops, blueEStops := arena.Plc.GetTeamEStops()
			redAStops, blueAStops := arena.Plc.GetTeamAStops()
			arena.handleTeamStop("R1", redEStops[0], redAStops[0])
			arena.handleTeamStop("R2", redEStops[1], redAStops[1])
			arena.handleTeamStop("R3", redEStops[2], redAStops[2])
			arena.handleTeamStop("B1", blueEStops[0], blueAStops[0])
			arena.handleTeamStop("B2", blueEStops[1], blueAStops[1])
		arena.handleTeamStop("B3", blueEStops[2], blueAStops[2])
		redEthernets, blueEthernets := arena.Plc.GetEthernetConnected()
		arena.AllianceStations["R1"].Ethernet = redEthernets[0]
		arena.AllianceStations["R2"].Ethernet = redEthernets[1]
		arena.AllianceStations["R3"].Ethernet = redEthernets[2]
		arena.AllianceStations["B1"].Ethernet = blueEthernets[0]
		arena.AllianceStations["B2"].Ethernet = blueEthernets[1]
		arena.AllianceStations["B3"].Ethernet = blueEthernets[2]

		// Handle in-match PLC functions.
		redScore := &arena.RedRealtimeScore.CurrentScore
		oldRedScore := *redScore
		blueScore := &arena.BlueRealtimeScore.CurrentScore
		oldBlueScore := *blueScore
		matchStartTime := arena.MatchStartTime
		currentTime := time.Now()
		teleopGracePeriod := matchStartTime.Add(
			game.GetDurationToTeleopEnd() + game.SpeakerTeleopGracePeriodSec*time.Second,
		)
		inGracePeriod := arena.MatchState == PostMatch && currentTime.Before(teleopGracePeriod)

		redAllianceReady := arena.checkAllianceStationsReady("R1", "R2", "R3") == nil
		blueAllianceReady := arena.checkAllianceStationsReady("B1", "B2", "B3") == nil

		// Handle the evergreen PLC functions: stack lights, stack buzzer, and field reset light.
		switch arena.MatchState {
		case PreMatch:
			if arena.lastMatchState != PreMatch {
				arena.Plc.SetFieldResetLight(true)
			}
			fallthrough
		case TimeoutActive:
			fallthrough
		case PostTimeout:
			// Set the stack light state -- solid alliance color(s) if robots are not connected, solid orange if scores are
			// not input, or blinking green if ready.
			greenStackLight := redAllianceReady && blueAllianceReady && arena.Plc.GetCycleState(2, 0, 2)
			arena.Plc.SetStackLights(!redAllianceReady, !blueAllianceReady, false, greenStackLight)
			arena.Plc.SetStackBuzzer(redAllianceReady && blueAllianceReady)

			// Turn off lights if all teams become ready.
			if redAllianceReady && blueAllianceReady {
				arena.FieldReset = false
				arena.Plc.SetFieldResetLight(false)
				if arena.CurrentMatch.FieldReadyAt.IsZero() {
					arena.CurrentMatch.FieldReadyAt = time.Now()
				}
			}
		case PostMatch:
			if arena.FieldReset {
				arena.Plc.SetFieldResetLight(true)
			}
			scoreReady := arena.RedRealtimeScore.FoulsCommitted && arena.BlueRealtimeScore.FoulsCommitted &&
				arena.alliancePostMatchScoreReady("red") && arena.alliancePostMatchScoreReady("blue")
			arena.Plc.SetStackLights(false, false, !scoreReady, false)
		case AutoPeriod, PausePeriod, TeleopPeriod:
			arena.Plc.SetStackBuzzer(false)
			arena.Plc.SetStackLights(!redAllianceReady, !blueAllianceReady, false, true)
		}

		// Get all the game-specific inputs and update the score.
		redAmplifyButton, redCoopButton, blueAmplifyButton, blueCoopButton := arena.Plc.GetAmpButtons()
		redAmpNoteCount, redSpeakerNoteCount, blueAmpNoteCount, blueSpeakerNoteCount := arena.Plc.GetAmpSpeakerNoteCounts()
		redAmpSpeaker := &arena.RedRealtimeScore.CurrentScore.AmpSpeaker
		blueAmpSpeaker := &arena.BlueRealtimeScore.CurrentScore.AmpSpeaker
		redAmpSpeaker.UpdateState(
			redAmpNoteCount, redSpeakerNoteCount, redAmplifyButton, redCoopButton, matchStartTime, currentTime,
		)
		blueAmpSpeaker.UpdateState(
			blueAmpNoteCount, blueSpeakerNoteCount, blueAmplifyButton, blueCoopButton, matchStartTime, currentTime,
		)
		if !oldRedScore.Equals(redScore) || !oldBlueScore.Equals(blueScore) {
			arena.RealtimeScoreNotifier.Notify()
		}

		// Handle the amp outputs.
		redAmplifiedTimeRemaining := redAmpSpeaker.AmplifiedTimeRemaining(currentTime)
		blueAmplifiedTimeRemaining := blueAmpSpeaker.AmplifiedTimeRemaining(currentTime)
		if arena.MatchState == AutoPeriod || arena.MatchState == PausePeriod || arena.MatchState == TeleopPeriod {
			redLowAmpLight := redAmpSpeaker.BankedAmpNotes >= 1
			redHighAmpLight := redAmpSpeaker.BankedAmpNotes >= 2
			redCoopAmpLight := redAmpSpeaker.CoopActivated
			if redAmplifiedTimeRemaining > 0 {
				redLowAmpLight = int(redAmplifiedTimeRemaining*2)%2 == 0
				redHighAmpLight = !redLowAmpLight
			}

			blueLowAmpLight := blueAmpSpeaker.BankedAmpNotes >= 1
			blueHighAmpLight := blueAmpSpeaker.BankedAmpNotes >= 2
			blueCoopAmpLight := blueAmpSpeaker.CoopActivated
			if blueAmplifiedTimeRemaining > 0 {
				blueLowAmpLight = int(blueAmplifiedTimeRemaining*4)%2 == 0
				blueHighAmpLight = !blueLowAmpLight
			}

			arena.Plc.SetAmpLights(
				redLowAmpLight, redHighAmpLight, redCoopAmpLight, blueLowAmpLight, blueHighAmpLight, blueCoopAmpLight,
			)
		} else if arena.MatchState == PostMatch {
			arena.Plc.SetAmpLights(false, false, false, false, false, false)
		}

		// Handle the speaker outputs.
		arena.Plc.SetSpeakerMotors(
			arena.MatchState == AutoPeriod || arena.MatchState == PausePeriod || arena.MatchState == TeleopPeriod ||
				inGracePeriod,
		)
		arena.Plc.SetSpeakerLights(
			redAmplifiedTimeRemaining > 0 && arena.MatchState != PostMatch,
			blueAmplifiedTimeRemaining > 0 && arena.MatchState != PostMatch,
		)

		// Handle the subwoofer outputs.
		arena.Plc.SetSubwooferCountdown(
			redAmplifiedTimeRemaining > 0 && arena.MatchState != PostMatch,
			blueAmplifiedTimeRemaining > 0 && arena.MatchState != PostMatch,
		)
		arena.Plc.SetPostMatchSubwooferLights(inGracePeriod)
	}
}

func (arena *Arena) handleTeamStop(station string, eStopState, aStopState bool) {
	allianceStation := arena.AllianceStations[station]
	if eStopState {
		allianceStation.EStop = true
	} else if arena.MatchTimeSec() == 0 {
		// Keep the E-stop latched until the match is over.
		allianceStation.EStop = false
	}
	if aStopState {
		allianceStation.AStop = true
	} else if arena.MatchState != AutoPeriod {
		// Keep the A-stop latched until the autonomous period is over.
		allianceStation.AStop = false
		allianceStation.aStopReset = true
	}
}

func (arena *Arena) handleSounds(matchTimeSec float64) {
	if arena.MatchState == PreMatch || arena.MatchState == TimeoutActive || arena.MatchState == PostTimeout {
		// Only apply this logic during a match.
		return
	}

	for _, sound := range game.MatchSounds {
		if sound.MatchTimeSec < 0 {
			// Skip sounds with negative timestamps; they are meant to only be triggered explicitly.
			continue
		}
		if _, ok := arena.soundsPlayed[sound]; !ok {
			if matchTimeSec > sound.MatchTimeSec && matchTimeSec-sound.MatchTimeSec < 1 {
				arena.playSound(sound.Name)
				arena.soundsPlayed[sound] = struct{}{}
			}
		}
	}
}

func (arena *Arena) playSound(name string) {
	if !arena.MuteMatchSounds {
		arena.PlaySoundNotifier.NotifyWithMessage(name)
	}
}

func (arena *Arena) alliancePostMatchScoreReady(alliance string) bool {
	numPanels := arena.ScoringPanelRegistry.GetNumPanels(alliance)
	return numPanels > 0 && arena.ScoringPanelRegistry.GetNumScoreCommitted(alliance) >= numPanels
}

// Performs any actions that need to run at the interval specified by periodicTaskPeriodSec.
func (arena *Arena) runPeriodicTasks() {
	arena.updateEarlyLateMessage()
	arena.purgeDisconnectedDisplays()
}
