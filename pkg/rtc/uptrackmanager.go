package rtc

import (
	"errors"
	"sync"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/utils"
)

var (
	ErrSubscriptionPermissionNeedsId = errors.New("either participant identity or SID needed")
)

type UpTrackManagerParams struct {
	SID              livekit.ParticipantID
	Logger           logger.Logger
	VersionGenerator utils.TimedVersionGenerator
}

// UpTrackManager manages all uptracks from a participant
type UpTrackManager struct {
	params UpTrackManagerParams

	closed bool

	// publishedTracks that participant is publishing
	publishedTracks               map[livekit.TrackID]types.MediaTrack
	subscriptionPermission        *livekit.SubscriptionPermission
	subscriptionPermissionVersion *utils.TimedVersion
	// subscriber permission for published tracks
	subscriberPermissions map[livekit.ParticipantIdentity]*livekit.TrackPermission // subscriberIdentity => *livekit.TrackPermission

	lock sync.RWMutex

	// callbacks & handlers
	onClose        func()
	onTrackUpdated func(track types.MediaTrack)
}

func NewUpTrackManager(params UpTrackManagerParams) *UpTrackManager {
	return &UpTrackManager{
		params:          params,
		publishedTracks: make(map[livekit.TrackID]types.MediaTrack),
	}
}

func (u *UpTrackManager) Start() {
}

func (u *UpTrackManager) Close(willBeResumed bool) {
	u.lock.Lock()
	u.closed = true
	notify := len(u.publishedTracks) == 0
	u.lock.Unlock()

	// remove all subscribers
	for _, t := range u.GetPublishedTracks() {
		t.ClearAllReceivers(willBeResumed)
	}

	if notify && u.onClose != nil {
		u.onClose()
	}
}

func (u *UpTrackManager) OnUpTrackManagerClose(f func()) {
	u.onClose = f
}

func (u *UpTrackManager) ToProto() []*livekit.TrackInfo {
	u.lock.RLock()
	defer u.lock.RUnlock()

	var trackInfos []*livekit.TrackInfo
	for _, t := range u.publishedTracks {
		trackInfos = append(trackInfos, t.ToProto())
	}

	return trackInfos
}

func (u *UpTrackManager) OnPublishedTrackUpdated(f func(track types.MediaTrack)) {
	u.onTrackUpdated = f
}

func (u *UpTrackManager) SetPublishedTrackMuted(trackID livekit.TrackID, muted bool) types.MediaTrack {
	u.lock.RLock()
	track := u.publishedTracks[trackID]
	u.lock.RUnlock()

	if track != nil {
		currentMuted := track.IsMuted()
		track.SetMuted(muted)

		if currentMuted != track.IsMuted() {
			u.params.Logger.Infow("publisher mute status changed", "trackID", trackID, "muted", track.IsMuted())
			if u.onTrackUpdated != nil {
				u.onTrackUpdated(track)
			}
		}
	}

	return track
}

func (u *UpTrackManager) GetPublishedTrack(trackID livekit.TrackID) types.MediaTrack {
	u.lock.RLock()
	defer u.lock.RUnlock()

	return u.getPublishedTrackLocked(trackID)
}

func (u *UpTrackManager) GetPublishedTracks() []types.MediaTrack {
	u.lock.RLock()
	defer u.lock.RUnlock()

	tracks := make([]types.MediaTrack, 0, len(u.publishedTracks))
	for _, t := range u.publishedTracks {
		tracks = append(tracks, t)
	}
	return tracks
}

func (u *UpTrackManager) UpdateSubscriptionPermission(
	subscriptionPermission *livekit.SubscriptionPermission,
	timedVersion *livekit.TimedVersion,
	resolverByIdentity func(participantIdentity livekit.ParticipantIdentity) types.LocalParticipant,
	resolverBySid func(participantID livekit.ParticipantID) types.LocalParticipant,
) error {
	u.lock.Lock()
	if timedVersion != nil {
		// it's possible for permission updates to come from another node. In that case
		// they would be the authority for this participant's permissions
		// we do not want to initialize subscriptionPermissionVersion too early since if another machine is the
		// owner for the data, we'd prefer to use their TimedVersion
		if u.subscriptionPermissionVersion != nil {
			tv := utils.NewTimedVersionFromProto(timedVersion)
			// ignore older version
			if !tv.After(u.subscriptionPermissionVersion) {
				perms := ""
				if u.subscriptionPermission != nil {
					perms = u.subscriptionPermission.String()
				}
				u.params.Logger.Infow(
					"skipping older subscription permission version",
					"existingValue", perms,
					"existingVersion", u.subscriptionPermissionVersion.ToProto().String(),
					"requestingValue", subscriptionPermission.String(),
					"requestingVersion", timedVersion.String(),
				)
				u.lock.Unlock()
				return nil
			}
			u.subscriptionPermissionVersion.Update(tv)
		} else {
			u.subscriptionPermissionVersion = utils.NewTimedVersionFromProto(timedVersion)
		}
	} else {
		// for requests coming from the current node, use local versions
		tv := u.params.VersionGenerator.New()
		// use current time as the new/updated version
		if u.subscriptionPermissionVersion == nil {
			u.subscriptionPermissionVersion = tv
		} else {
			u.subscriptionPermissionVersion.Update(tv)
		}
	}

	// store as is for use when migrating
	u.subscriptionPermission = subscriptionPermission
	if subscriptionPermission == nil {
		u.params.Logger.Debugw(
			"updating subscription permission, setting to nil",
			"version", u.subscriptionPermissionVersion.ToProto().String(),
		)
		// possible to get a nil when migrating
		u.lock.Unlock()
		return nil
	}

	u.params.Logger.Debugw(
		"updating subscription permission",
		"permissions", u.subscriptionPermission.String(),
		"version", u.subscriptionPermissionVersion.ToProto().String(),
	)
	if err := u.parseSubscriptionPermissionsLocked(subscriptionPermission, func(pID livekit.ParticipantID) types.LocalParticipant {
		u.lock.Unlock()
		p := resolverBySid(pID)
		u.lock.Lock()
		return p
	}); err != nil {
		// when failed, do not override previous permissions
		u.params.Logger.Errorw("failed updating subscription permission", err)
		u.lock.Unlock()
		return err
	}
	u.lock.Unlock()

	u.maybeRevokeSubscriptions(resolverByIdentity)

	return nil
}

func (u *UpTrackManager) SubscriptionPermission() (*livekit.SubscriptionPermission, *livekit.TimedVersion) {
	u.lock.RLock()
	defer u.lock.RUnlock()

	if u.subscriptionPermissionVersion == nil {
		return nil, nil
	}

	return u.subscriptionPermission, u.subscriptionPermissionVersion.ToProto()
}

func (u *UpTrackManager) HasPermission(trackID livekit.TrackID, subIdentity livekit.ParticipantIdentity) bool {
	u.lock.RLock()
	defer u.lock.RUnlock()

	return u.hasPermissionLocked(trackID, subIdentity)
}

func (u *UpTrackManager) UpdateVideoLayers(updateVideoLayers *livekit.UpdateVideoLayers) error {
	track := u.GetPublishedTrack(livekit.TrackID(updateVideoLayers.TrackSid))
	if track == nil {
		u.params.Logger.Warnw("could not find track", nil, "trackID", livekit.TrackID(updateVideoLayers.TrackSid))
		return errors.New("could not find published track")
	}

	track.UpdateVideoLayers(updateVideoLayers.Layers)
	if u.onTrackUpdated != nil {
		u.onTrackUpdated(track)
	}

	return nil
}

func (u *UpTrackManager) AddPublishedTrack(track types.MediaTrack) {
	u.lock.Lock()
	if _, ok := u.publishedTracks[track.ID()]; !ok {
		u.publishedTracks[track.ID()] = track
	}
	u.lock.Unlock()
	u.params.Logger.Debugw("added published track", "trackID", track.ID(), "trackInfo", track.ToProto().String())

	track.AddOnClose(func() {
		notifyClose := false

		// cleanup
		u.lock.Lock()
		trackID := track.ID()
		delete(u.publishedTracks, trackID)
		// not modifying subscription permissions, will get reset on next update from participant

		if u.closed && len(u.publishedTracks) == 0 {
			notifyClose = true
		}
		u.lock.Unlock()

		if notifyClose && u.onClose != nil {
			u.onClose()
		}
	})
}

func (u *UpTrackManager) RemovePublishedTrack(track types.MediaTrack, willBeResumed bool, shouldClose bool) {
	if shouldClose {
		track.Close(willBeResumed)
	} else {
		track.ClearAllReceivers(willBeResumed)
	}
	u.lock.Lock()
	delete(u.publishedTracks, track.ID())
	u.lock.Unlock()
}

func (u *UpTrackManager) getPublishedTrackLocked(trackID livekit.TrackID) types.MediaTrack {
	return u.publishedTracks[trackID]
}

func (u *UpTrackManager) parseSubscriptionPermissionsLocked(
	subscriptionPermission *livekit.SubscriptionPermission,
	resolver func(participantID livekit.ParticipantID) types.LocalParticipant,
) error {
	// every update overrides the existing

	// all_participants takes precedence
	if subscriptionPermission.AllParticipants {
		// everything is allowed, nothing else to do
		u.subscriberPermissions = nil
		return nil
	}

	// per participant permissions
	subscriberPermissions := make(map[livekit.ParticipantIdentity]*livekit.TrackPermission)
	for _, trackPerms := range subscriptionPermission.TrackPermissions {
		subscriberIdentity := livekit.ParticipantIdentity(trackPerms.ParticipantIdentity)
		if subscriberIdentity == "" {
			if trackPerms.ParticipantSid == "" {
				return ErrSubscriptionPermissionNeedsId
			}

			sub := resolver(livekit.ParticipantID(trackPerms.ParticipantSid))
			if sub == nil {
				u.params.Logger.Warnw("could not find subscriber for permissions update", nil, "subscriberID", trackPerms.ParticipantSid)
				continue
			}

			subscriberIdentity = sub.Identity()
		} else {
			if trackPerms.ParticipantSid != "" {
				sub := resolver(livekit.ParticipantID(trackPerms.ParticipantSid))
				if sub != nil && sub.Identity() != subscriberIdentity {
					u.params.Logger.Errorw("participant identity mismatch", nil, "expected", subscriberIdentity, "got", sub.Identity())
				}
				if sub == nil {
					u.params.Logger.Warnw("could not find subscriber for permissions update", nil, "subscriberID", trackPerms.ParticipantSid)
				}
			}
		}

		subscriberPermissions[subscriberIdentity] = trackPerms
	}

	u.subscriberPermissions = subscriberPermissions

	return nil
}

func (u *UpTrackManager) hasPermissionLocked(trackID livekit.TrackID, subscriberIdentity livekit.ParticipantIdentity) bool {
	if u.subscriberPermissions == nil {
		return true
	}

	perms, ok := u.subscriberPermissions[subscriberIdentity]
	if !ok {
		return false
	}

	if perms.AllTracks {
		return true
	}

	for _, sid := range perms.TrackSids {
		if livekit.TrackID(sid) == trackID {
			return true
		}
	}

	return false
}

// returns a list of participants that are allowed to subscribe to the track. if nil is returned, it means everyone is
// allowed to subscribe to this track
func (u *UpTrackManager) getAllowedSubscribersLocked(trackID livekit.TrackID) []livekit.ParticipantIdentity {
	if u.subscriberPermissions == nil {
		return nil
	}

	allowed := make([]livekit.ParticipantIdentity, 0)
	for subscriberIdentity, perms := range u.subscriberPermissions {
		if perms.AllTracks {
			allowed = append(allowed, subscriberIdentity)
			continue
		}

		for _, sid := range perms.TrackSids {
			if livekit.TrackID(sid) == trackID {
				allowed = append(allowed, subscriberIdentity)
				break
			}
		}
	}

	return allowed
}

func (u *UpTrackManager) maybeRevokeSubscriptions(resolver func(participantIdentity livekit.ParticipantIdentity) types.LocalParticipant) {
	u.lock.Lock()
	defer u.lock.Unlock()

	for trackID, track := range u.publishedTracks {
		allowed := u.getAllowedSubscribersLocked(trackID)
		if allowed == nil {
			// no restrictions
			continue
		}

		track.RevokeDisallowedSubscribers(allowed)
	}
}

func (u *UpTrackManager) DebugInfo() map[string]interface{} {
	info := map[string]interface{}{}
	publishedTrackInfo := make(map[livekit.TrackID]interface{})

	u.lock.RLock()
	for trackID, track := range u.publishedTracks {
		if mt, ok := track.(*MediaTrack); ok {
			publishedTrackInfo[trackID] = mt.DebugInfo()
		} else {
			publishedTrackInfo[trackID] = map[string]interface{}{
				"ID":       track.ID(),
				"Kind":     track.Kind().String(),
				"PubMuted": track.IsMuted(),
			}
		}
	}
	u.lock.RUnlock()

	info["PublishedTracks"] = publishedTrackInfo

	return info
}
