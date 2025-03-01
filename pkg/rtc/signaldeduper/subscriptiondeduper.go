package signaldeduper

import (
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/rtc/types"
)

const (
	dupeBarrierDuration = 5 * time.Second
)

// --------------------------------------------------

type subscriptionSetting struct {
	isEnabled         bool
	trackSettingsSeen bool
	quality           livekit.VideoQuality
	width             uint32
	height            uint32
	fps               uint32
	priority          uint32
}

func subscriptionSettingFromUpdateSubscription(us *livekit.UpdateSubscription, existing *subscriptionSetting) *subscriptionSetting {
	var ss subscriptionSetting
	if existing != nil {
		ss = *existing
	}
	ss.isEnabled = us.Subscribe
	return &ss

}

func subscriptionSettingFromUpdateTrackSettings(uts *livekit.UpdateTrackSettings) *subscriptionSetting {
	return &subscriptionSetting{
		isEnabled:         !uts.Disabled,
		trackSettingsSeen: true,
		quality:           uts.Quality,
		width:             uts.Width,
		height:            uts.Height,
		fps:               uts.Fps,
		priority:          uts.Priority,
	}
}

func (s *subscriptionSetting) Equal(other *subscriptionSetting) bool {
	return s.isEnabled == other.isEnabled &&
		s.trackSettingsSeen == other.trackSettingsSeen &&
		s.quality == other.quality &&
		s.width == other.width &&
		s.height == other.height &&
		s.fps == other.fps &&
		s.priority == other.priority
}

// --------------------------------------------------

type subscriptionState struct {
	setting         *subscriptionSetting
	lastNonDupeTime time.Time
}

type SubscriptionDeduper struct {
	logger logger.Logger

	lock                      sync.RWMutex
	participantsSubscriptions map[livekit.ParticipantKey]map[livekit.TrackID]*subscriptionState
}

func NewSubscriptionDeduper(logger logger.Logger) types.SignalDeduper {
	return &SubscriptionDeduper{
		logger:                    logger,
		participantsSubscriptions: make(map[livekit.ParticipantKey]map[livekit.TrackID]*subscriptionState),
	}
}

func (s *SubscriptionDeduper) Dedupe(participantKey livekit.ParticipantKey, req *livekit.SignalRequest) bool {
	isDupe := false
	switch msg := req.Message.(type) {
	case *livekit.SignalRequest_Subscription:
		isDupe = s.updateSubscriptionsFromUpdateSubscription(participantKey, msg.Subscription)
	case *livekit.SignalRequest_TrackSetting:
		isDupe = s.updateSubscriptionsFromUpdateTrackSettings(participantKey, msg.TrackSetting)
	default:
		return false
	}
	s.logger.Infow("subscription deduper received message", "participantKey", participantKey, "update", req.String(), "isDupe", isDupe)

	return isDupe
}

func (s *SubscriptionDeduper) ParticipantClosed(participantKey livekit.ParticipantKey) {
	s.lock.Lock()
	defer s.lock.Unlock()

	delete(s.participantsSubscriptions, participantKey)
}

func (s *SubscriptionDeduper) updateSubscriptionsFromUpdateSubscription(
	participantKey livekit.ParticipantKey,
	us *livekit.UpdateSubscription,
) bool {
	isDupe := true

	s.lock.Lock()
	defer s.lock.Unlock()

	numTracks := len(us.TrackSids)
	for _, pt := range us.ParticipantTracks {
		numTracks += len(pt.TrackSids)
	}
	trackIDs := make(map[livekit.TrackID]bool, numTracks)
	for _, trackSid := range us.TrackSids {
		trackIDs[livekit.TrackID(trackSid)] = true
	}
	for _, pt := range us.ParticipantTracks {
		for _, trackSid := range pt.TrackSids {
			trackIDs[livekit.TrackID(trackSid)] = true
		}
	}

	for trackID := range trackIDs {
		var existingSetting *subscriptionSetting
		existingState := s.getSubscriptionState(participantKey, trackID)
		if existingState != nil {
			existingSetting = existingState.setting
		}

		newSetting := subscriptionSettingFromUpdateSubscription(us, existingSetting)

		isTrackDupe := s.detectDupe(participantKey, trackID, newSetting)
		if !isTrackDupe {
			isDupe = false
		}
	}

	return isDupe
}

func (s *SubscriptionDeduper) updateSubscriptionsFromUpdateTrackSettings(
	participantKey livekit.ParticipantKey,
	uts *livekit.UpdateTrackSettings,
) bool {
	isDupe := true

	s.lock.Lock()
	defer s.lock.Unlock()

	newSetting := subscriptionSettingFromUpdateTrackSettings(uts)
	for _, trackSid := range uts.TrackSids {
		isTrackDupe := s.detectDupe(participantKey, livekit.TrackID(trackSid), newSetting)
		if !isTrackDupe {
			isDupe = false
		}
	}

	return isDupe
}

func (s *SubscriptionDeduper) getOrCreateParticipantSubscriptions(
	participantKey livekit.ParticipantKey,
) map[livekit.TrackID]*subscriptionState {
	participantSubscriptions := s.participantsSubscriptions[participantKey]
	if participantSubscriptions == nil {
		participantSubscriptions = make(map[livekit.TrackID]*subscriptionState)
		s.participantsSubscriptions[participantKey] = participantSubscriptions
	}

	return participantSubscriptions
}

func (s *SubscriptionDeduper) detectDupe(
	participantKey livekit.ParticipantKey,
	trackID livekit.TrackID,
	updatedSetting *subscriptionSetting,
) bool {
	isDupe := true
	state := s.getSubscriptionState(participantKey, trackID)
	if state == nil || !state.setting.Equal(updatedSetting) {
		// new track seen or subscription setting change
		state = &subscriptionState{
			setting:         updatedSetting,
			lastNonDupeTime: time.Now(),
		}
		isDupe = false
	}

	if isDupe && time.Since(state.lastNonDupeTime) > dupeBarrierDuration {
		state.lastNonDupeTime = time.Now()
		isDupe = false
	}

	if !isDupe {
		s.setSubscriptionState(participantKey, trackID, state)
	}

	return isDupe
}

func (s *SubscriptionDeduper) getSubscriptionState(participantKey livekit.ParticipantKey, trackID livekit.TrackID) *subscriptionState {
	participantSubscriptions := s.getOrCreateParticipantSubscriptions(participantKey)
	return participantSubscriptions[trackID]
}

func (s *SubscriptionDeduper) setSubscriptionState(participantKey livekit.ParticipantKey, trackID livekit.TrackID, state *subscriptionState) {
	participantSubscriptions := s.getOrCreateParticipantSubscriptions(participantKey)
	participantSubscriptions[trackID] = state
}
