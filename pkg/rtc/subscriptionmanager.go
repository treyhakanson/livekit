/*
 * Copyright 2023 LiveKit, Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package rtc

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v3/pkg/rtcerr"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// using var instead of const to override in tests
var (
	reconcileInterval = 3 * time.Second
	// amount of time to give up if a track or publisher isn't found
	// ensuring this is longer than iceFailedTimeout so we are certain the participant won't return
	notFoundTimeout = iceFailedTimeout
	// amount of time to try otherwise before flagging subscription as failed
	subscriptionTimeout = iceFailedTimeout
)

type SubscriptionManagerParams struct {
	Logger              logger.Logger
	Participant         types.LocalParticipant
	TrackResolver       types.MediaTrackResolver
	OnTrackSubscribed   func(subTrack types.SubscribedTrack)
	OnTrackUnsubscribed func(subTrack types.SubscribedTrack)
	OnSubcriptionError  func(trackID livekit.TrackID)
	Telemetry           telemetry.TelemetryService
}

// SubscriptionManager manages a participant's subscriptions
type SubscriptionManager struct {
	params        SubscriptionManagerParams
	lock          sync.RWMutex
	subscriptions map[livekit.TrackID]*trackSubscription
	subscribedTo  map[livekit.ParticipantID]map[livekit.TrackID]struct{}
	reconcileCh   chan livekit.TrackID
	closeCh       chan struct{}
	doneCh        chan struct{}

	onSubscribeStatusChanged func(publisherID livekit.ParticipantID, subscribed bool)
}

func NewSubscriptionManager(params SubscriptionManagerParams) *SubscriptionManager {
	m := &SubscriptionManager{
		params:        params,
		subscriptions: make(map[livekit.TrackID]*trackSubscription),
		subscribedTo:  make(map[livekit.ParticipantID]map[livekit.TrackID]struct{}),
		reconcileCh:   make(chan livekit.TrackID, 50),
		closeCh:       make(chan struct{}),
		doneCh:        make(chan struct{}),
	}

	go m.reconcileWorker()
	return m
}

func (m *SubscriptionManager) Close(willBeResumed bool) {
	m.lock.Lock()
	if m.isClosed() {
		m.lock.Unlock()
		return
	}
	close(m.closeCh)
	m.lock.Unlock()

	<-m.doneCh

	subTracks := m.GetSubscribedTracks()
	downTracksToClose := make([]*sfu.DownTrack, 0, len(subTracks))
	for _, st := range subTracks {
		dt := st.DownTrack()
		// nil check exists primarily for tests
		if dt != nil {
			downTracksToClose = append(downTracksToClose, st.DownTrack())
		}
	}

	for _, dt := range downTracksToClose {
		dt.CloseWithFlush(!willBeResumed)
	}
}

func (m *SubscriptionManager) isClosed() bool {
	select {
	case <-m.closeCh:
		return true
	default:
		return false
	}
}

func (m *SubscriptionManager) SubscribeToTrack(trackID livekit.TrackID) {
	m.lock.Lock()
	sub, ok := m.subscriptions[trackID]
	if !ok {
		sLogger := m.params.Logger.WithValues(
			"trackID", trackID,
		)
		sub = newTrackSubscription(m.params.Participant.ID(), trackID, sLogger)
		m.subscriptions[trackID] = sub
	}
	desireChanged := sub.setDesired(true)
	m.lock.Unlock()
	if desireChanged {
		sub.logger.Infow("subscribing to track")
	}

	// always reconcile, since SubscribeToTrack could be called when the track is ready
	m.queueReconcile(trackID)
}

func (m *SubscriptionManager) UnsubscribeFromTrack(trackID livekit.TrackID) {
	m.lock.Lock()
	sub, ok := m.subscriptions[trackID]
	m.lock.Unlock()
	if !ok {
		return
	}

	if sub.setDesired(false) {
		sub.logger.Infow("unsubscribing from track")
		m.queueReconcile(trackID)
	}
}

func (m *SubscriptionManager) GetSubscribedTracks() []types.SubscribedTrack {
	m.lock.RLock()
	defer m.lock.RUnlock()

	tracks := make([]types.SubscribedTrack, 0, len(m.subscriptions))
	for _, t := range m.subscriptions {
		st := t.getSubscribedTrack()
		if st != nil {
			tracks = append(tracks, st)
		}
	}
	return tracks
}

func (m *SubscriptionManager) HasSubscriptions() bool {
	m.lock.RLock()
	defer m.lock.RUnlock()
	for _, s := range m.subscriptions {
		if s.isDesired() {
			return true
		}
	}
	return false
}

func (m *SubscriptionManager) GetSubscribedParticipants() []livekit.ParticipantID {
	m.lock.RLock()
	defer m.lock.RUnlock()

	var participantIDs []livekit.ParticipantID
	for pID := range m.subscribedTo {
		participantIDs = append(participantIDs, pID)
	}
	return participantIDs
}

func (m *SubscriptionManager) IsSubscribedTo(participantID livekit.ParticipantID) bool {
	m.lock.RLock()
	defer m.lock.RUnlock()

	_, ok := m.subscribedTo[participantID]
	return ok
}

func (m *SubscriptionManager) UpdateSubscribedTrackSettings(trackID livekit.TrackID, settings *livekit.UpdateTrackSettings) {
	m.lock.Lock()
	sub, ok := m.subscriptions[trackID]
	if !ok {
		sLogger := m.params.Logger.WithValues(
			"trackID", trackID,
		)
		sub = newTrackSubscription(m.params.Participant.ID(), trackID, sLogger)
		m.subscriptions[trackID] = sub
	}
	m.lock.Unlock()

	sub.setSettings(settings)
}

// OnSubscribeStatusChanged callback will be notified when a participant subscribes or unsubscribes to another participant
// it will only fire once per publisher. If current participant is subscribed to multiple tracks from another, this
// callback will only fire once.
func (m *SubscriptionManager) OnSubscribeStatusChanged(fn func(publisherID livekit.ParticipantID, subscribed bool)) {
	m.lock.Lock()
	m.onSubscribeStatusChanged = fn
	m.lock.Unlock()
}

func (m *SubscriptionManager) WaitUntilSubscribed(timeout time.Duration) error {
	expiresAt := time.Now().Add(timeout)
	for expiresAt.After(time.Now()) {
		allSubscribed := true
		m.lock.RLock()
		for _, sub := range m.subscriptions {
			if sub.needsSubscribe() {
				allSubscribed = false
				break
			}
		}
		m.lock.RUnlock()
		if allSubscribed {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}

	return context.DeadlineExceeded
}

func (m *SubscriptionManager) canReconcile() bool {
	p := m.params.Participant
	if m.isClosed() || p.IsClosed() || p.IsDisconnected() {
		return false
	}
	return true
}

func (m *SubscriptionManager) reconcileSubscriptions() {
	var needsToReconcile []*trackSubscription
	m.lock.RLock()
	for _, sub := range m.subscriptions {
		if sub.needsSubscribe() || sub.needsUnsubscribe() || sub.needsBind() {
			needsToReconcile = append(needsToReconcile, sub)
		}
	}
	m.lock.RUnlock()

	for _, s := range needsToReconcile {
		m.reconcileSubscription(s)
	}
}

func (m *SubscriptionManager) reconcileSubscription(s *trackSubscription) {
	if !m.canReconcile() {
		return
	}
	if s.needsSubscribe() {
		if s.numAttempts.Load() == 0 {
			m.params.Telemetry.TrackSubscribeRequested(
				context.Background(),
				m.params.Participant.ID(),
				&livekit.TrackInfo{
					Sid: string(s.trackID),
				},
			)
		}
		if err := m.subscribe(s); err != nil {
			s.recordAttempt(false)

			switch err {
			case ErrNoTrackPermission, ErrNoSubscribePermission, ErrNoReceiver, ErrNotOpen, ErrTrackNotAttached:
				// these are errors that are outside of our control, so we'll keep trying
				// - ErrNoTrackPermission: publisher did not grant subscriber permission, may change any moment
				// - ErrNoSubscribePermission: participant was not granted canSubscribe, may change any moment
				// - ErrNoReceiver: Track is in the process of closing (another local track published to the same instance)
				// - ErrTrackNotAttached: Remote Track that is not attached, but may be attached later
				// - ErrNotOpen: Track is closing or already closed
				// We'll still log an event to reflect this in telemetry since it's been too long
				if s.durationSinceStart() > subscriptionTimeout {
					s.maybeRecordError(m.params.Telemetry, m.params.Participant.ID(), err, true)
				}
			case ErrTrackNotFound:
				// source track was never published or closed
				// if after timeout, we'd unsubscribe from it.
				// this is the *only* case we'd change desired state
				if s.durationSinceStart() > notFoundTimeout {
					s.maybeRecordError(m.params.Telemetry, m.params.Participant.ID(), err, true)
					s.logger.Infow("unsubscribing track since track isn't available", "error", err)
					s.setDesired(false)
					m.queueReconcile(s.trackID)
				}
			default:
				// all other errors
				if s.durationSinceStart() > subscriptionTimeout {
					s.logger.Errorw("failed to subscribe, triggering error handler", err,
						"attempt", s.numAttempts.Load(),
					)
					s.maybeRecordError(m.params.Telemetry, m.params.Participant.ID(), err, false)
					m.params.OnSubcriptionError(s.trackID)
				} else {
					s.logger.Debugw("failed to subscribe, retrying",
						"error", err,
						"attempt", s.numAttempts.Load(),
					)
				}
			}
		} else {
			s.recordAttempt(true)
		}

		return
	}

	if s.needsUnsubscribe() {
		if err := m.unsubscribe(s); err != nil {
			s.logger.Errorw("failed to unsubscribe", err)
		} else {
			// successfully unsubscribed, remove from map
			m.lock.Lock()
			if !s.isDesired() {
				delete(m.subscriptions, s.trackID)
			}
			m.lock.Unlock()
		}
		return
	}

	if s.needsBind() {
		// check bound status, notify error callback if it's not bound
		// if a publisher leaves or closes the source track, SubscribedTrack will be closed as well and it will go
		// back to needsSubscribe state
		if s.durationSinceStart() > subscriptionTimeout {
			s.logger.Errorw("track not bound after timeout", nil)
			s.maybeRecordError(m.params.Telemetry, m.params.Participant.ID(), ErrTrackNotBound, false)
			m.params.OnSubcriptionError(s.trackID)
		}
	}
}

// trigger an immediate reconcilation, when trackID is empty, will reconcile all subscriptions
func (m *SubscriptionManager) queueReconcile(trackID livekit.TrackID) {
	select {
	case m.reconcileCh <- trackID:
	default:
		// queue is full, will reconcile based on timer
	}
}

func (m *SubscriptionManager) reconcileWorker() {
	reconcileTicker := time.NewTicker(reconcileInterval)
	defer reconcileTicker.Stop()
	defer close(m.doneCh)

	for {
		select {
		case <-m.closeCh:
			return
		case <-reconcileTicker.C:
			m.reconcileSubscriptions()
		case trackID := <-m.reconcileCh:
			m.lock.RLock()
			s := m.subscriptions[trackID]
			m.lock.RUnlock()
			if s != nil {
				m.reconcileSubscription(s)
			} else {
				m.reconcileSubscriptions()
			}
		}
	}
}

func (m *SubscriptionManager) subscribe(s *trackSubscription) error {
	s.logger.Debugw("executing subscribe")

	if !m.params.Participant.CanSubscribe() {
		return ErrNoSubscribePermission
	}

	res := m.params.TrackResolver(m.params.Participant.Identity(), s.trackID)
	s.logger.Debugw("resolved track", "result", res)

	if res.TrackChangedNotifier != nil && s.setChangedNotifier(res.TrackChangedNotifier) {
		// set callback only when we haven't done it before
		// we set the observer before checking for existence of track, so that we may get notified
		// when the track becomes available
		res.TrackChangedNotifier.AddObserver(string(m.params.Participant.ID()), func() {
			m.queueReconcile(s.trackID)
		})
	}
	if res.TrackRemovedNotifier != nil && s.setRemovedNotifier(res.TrackRemovedNotifier) {
		res.TrackRemovedNotifier.AddObserver(string(m.params.Participant.ID()), func() {
			// source track removed, we would unsubscribe
			s.logger.Debugw("unsubscribing track since source track was removed")
			s.setDesired(false)
		})
	}

	track := res.Track
	if track == nil {
		return ErrTrackNotFound
	}

	// since hasPermission defaults to true, we will want to send a message to the client the first time
	// that we discover permissions were denied
	permChanged := s.setHasPermission(res.HasPermission)
	if permChanged {
		m.params.Participant.SubscriptionPermissionUpdate(s.getPublisherID(), s.trackID, res.HasPermission)
	}
	if !res.HasPermission {
		return ErrNoTrackPermission
	}

	s.setPublisher(res.PublisherIdentity, res.PublisherID)
	subTrack, err := track.AddSubscriber(m.params.Participant)
	if err != nil && err != errAlreadySubscribed {
		// ignore already subscribed error
		return err
	}
	if err == nil && subTrack != nil { // subTrack could be nil if already subscribed
		subTrack.OnClose(func(willBeResumed bool) {
			m.handleSubscribedTrackClose(s, willBeResumed)
		})
		subTrack.AddOnBind(func() {
			s.setBound()
			s.maybeRecordSuccess(m.params.Telemetry, m.params.Participant.ID())
		})
		s.setSubscribedTrack(subTrack)

		if subTrack.NeedsNegotiation() {
			m.params.Participant.Negotiate(false)
		}

		go m.params.OnTrackSubscribed(subTrack)
	}

	// add mark the participant as someone we've subscribed to
	firstSubscribe := false
	publisherID := s.getPublisherID()
	m.lock.Lock()
	pTracks := m.subscribedTo[publisherID]
	changedCB := m.onSubscribeStatusChanged
	if pTracks == nil {
		pTracks = make(map[livekit.TrackID]struct{})
		m.subscribedTo[publisherID] = pTracks
		firstSubscribe = true
	}
	pTracks[s.trackID] = struct{}{}
	m.lock.Unlock()

	if changedCB != nil && firstSubscribe {
		go changedCB(publisherID, true)
	}
	return nil
}

func (m *SubscriptionManager) unsubscribe(s *trackSubscription) error {
	s.logger.Debugw("executing unsubscribe")

	subTrack := s.getSubscribedTrack()
	if subTrack == nil {
		// already unsubscribed
		return nil
	}

	track := subTrack.MediaTrack()
	pID := m.params.Participant.ID()
	track.RemoveSubscriber(pID, false)

	return nil
}

// DownTrack closing is how the publisher signifies that the subscription is no longer fulfilled
// this could be due to a few reasons:
// - subscriber-initiated unsubscribe
// - UpTrack was closed
// - publisher revoked permissions for the participant
func (m *SubscriptionManager) handleSubscribedTrackClose(s *trackSubscription, willBeResumed bool) {
	s.logger.Debugw("subscribed track closed",
		"willBeResumed", willBeResumed,
	)
	wasBound := s.isBound()
	subTrack := s.getSubscribedTrack()
	if subTrack == nil {
		return
	}
	s.setSubscribedTrack(nil)

	// remove from subscribedTo
	publisherID := s.getPublisherID()
	lastSubscription := false
	m.lock.Lock()
	changedCB := m.onSubscribeStatusChanged
	pTracks := m.subscribedTo[publisherID]
	if pTracks != nil {
		delete(pTracks, s.trackID)
		if len(pTracks) == 0 {
			delete(m.subscribedTo, publisherID)
			lastSubscription = true
		}
	}
	m.lock.Unlock()
	if changedCB != nil && lastSubscription {
		go changedCB(publisherID, false)
	}

	go m.params.OnTrackUnsubscribed(subTrack)

	// trigger to decrement unsubscribed counter as long as track has been bound
	// Only log an analytics event when
	// * the participant isn't closing
	// * it's not a migration
	if wasBound {
		m.params.Telemetry.TrackUnsubscribed(
			context.Background(),
			m.params.Participant.ID(),
			&livekit.TrackInfo{Sid: string(s.trackID), Type: subTrack.MediaTrack().Kind()},
			!willBeResumed && !m.params.Participant.IsClosed(),
		)
	}

	if !willBeResumed {
		sender := subTrack.RTPSender()
		if sender != nil {
			s.logger.Debugw("removing PeerConnection track",
				"kind", subTrack.MediaTrack().Kind(),
			)

			if err := m.params.Participant.RemoveTrackFromSubscriber(sender); err != nil {
				if _, ok := err.(*rtcerr.InvalidStateError); !ok {
					// most of these are safe to ignore, since the track state might have already
					// been set to Inactive
					m.params.Logger.Debugw("could not remove remoteTrack from forwarder",
						"error", err,
						"publisher", subTrack.PublisherIdentity(),
						"publisherID", subTrack.PublisherID(),
					)
				}
			}
		}
	}

	if !willBeResumed {
		m.params.Participant.Negotiate(false)
	}
	m.queueReconcile(s.trackID)
}

type trackSubscription struct {
	subscriberID livekit.ParticipantID
	trackID      livekit.TrackID
	logger       logger.Logger

	lock              sync.RWMutex
	desired           bool
	publisherID       livekit.ParticipantID
	publisherIdentity livekit.ParticipantIdentity
	settings          *livekit.UpdateTrackSettings
	changedNotifier   types.ChangeNotifier
	removedNotifier   types.ChangeNotifier
	hasPermission     bool
	subscribedTrack   types.SubscribedTrack
	eventSent         atomic.Bool
	numAttempts       atomic.Int32
	bound             bool

	// the later of when subscription was requested or when the first failure was encountered
	// this timestamp determines when failures are reported
	subStartedAt atomic.Pointer[time.Time]
}

func newTrackSubscription(subscriberID livekit.ParticipantID, trackID livekit.TrackID, l logger.Logger) *trackSubscription {
	return &trackSubscription{
		subscriberID: subscriberID,
		trackID:      trackID,
		logger:       l,
		// default allow
		hasPermission: true,
	}
}

func (s *trackSubscription) setPublisher(publisherIdentity livekit.ParticipantIdentity, publisherID livekit.ParticipantID) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.publisherID = publisherID
	s.publisherIdentity = publisherIdentity
}

func (s *trackSubscription) getPublisherID() livekit.ParticipantID {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.publisherID
}

func (s *trackSubscription) setDesired(desired bool) bool {
	s.lock.Lock()
	if desired {
		// as long as user explicitly set it to desired
		// we'll reset the timer so it has sufficient time to reconcile
		t := time.Now()
		s.subStartedAt.Store(&t)
	}

	if s.desired == desired {
		s.lock.Unlock()
		return false
	}
	s.desired = desired
	s.lock.Unlock()

	// when no longer desired, we no longer care about change notifications
	if desired {
		// reset attempts
		s.numAttempts.Store(0)
	} else {
		s.setChangedNotifier(nil)
		s.setRemovedNotifier(nil)
	}
	return true
}

// set permission and return true if it has changed
func (s *trackSubscription) setHasPermission(perm bool) bool {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.hasPermission == perm {
		return false
	}
	s.hasPermission = perm
	return true
}

func (s *trackSubscription) getHasPermission() bool {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.hasPermission
}

func (s *trackSubscription) isDesired() bool {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.desired
}

func (s *trackSubscription) setSubscribedTrack(track types.SubscribedTrack) {
	s.lock.Lock()
	oldTrack := s.subscribedTrack
	s.subscribedTrack = track
	s.bound = false
	settings := s.settings
	s.lock.Unlock()

	if settings != nil && track != nil {
		s.logger.Debugw("restoring subscriber settings", "settings", settings)
		track.UpdateSubscriberSettings(settings)
	}
	if oldTrack != nil {
		oldTrack.OnClose(nil)
	}
}

func (s *trackSubscription) getSubscribedTrack() types.SubscribedTrack {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.subscribedTrack
}

func (s *trackSubscription) setChangedNotifier(notifier types.ChangeNotifier) bool {
	s.lock.Lock()
	if s.changedNotifier == notifier {
		s.lock.Unlock()
		return false
	}

	existing := s.changedNotifier
	s.changedNotifier = notifier
	s.lock.Unlock()

	if existing != nil {
		existing.RemoveObserver(string(s.subscriberID))
	}
	return true
}

func (s *trackSubscription) setRemovedNotifier(notifier types.ChangeNotifier) bool {
	s.lock.Lock()
	if s.removedNotifier == notifier {
		s.lock.Unlock()
		return false
	}

	existing := s.removedNotifier
	s.removedNotifier = notifier
	s.lock.Unlock()

	if existing != nil {
		existing.RemoveObserver(string(s.subscriberID))
	}
	return true
}

func (s *trackSubscription) setSettings(settings *livekit.UpdateTrackSettings) {
	s.lock.Lock()
	s.settings = settings
	subTrack := s.subscribedTrack
	s.lock.Unlock()
	if subTrack != nil {
		subTrack.UpdateSubscriberSettings(settings)
	}
}

// mark the subscription as bound - when we've received the client's answer
func (s *trackSubscription) setBound() {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.bound = true
}

func (s *trackSubscription) isBound() bool {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.bound
}

func (s *trackSubscription) recordAttempt(success bool) {
	if !success {
		if s.numAttempts.Load() == 0 {
			// on first failure, we'd want to start the timer
			t := time.Now()
			s.subStartedAt.Store(&t)
		}
		s.numAttempts.Add(1)
	} else {
		s.numAttempts.Store(0)
	}
}

func (s *trackSubscription) maybeRecordError(ts telemetry.TelemetryService, pID livekit.ParticipantID, err error, isUserError bool) {
	if s.eventSent.Swap(true) {
		return
	}

	ts.TrackSubscribeFailed(context.Background(), pID, s.trackID, err, isUserError)
}

func (s *trackSubscription) maybeRecordSuccess(ts telemetry.TelemetryService, pID livekit.ParticipantID) {
	subTrack := s.getSubscribedTrack()
	if subTrack == nil {
		return
	}
	mediaTrack := subTrack.MediaTrack()
	if mediaTrack == nil {
		return
	}

	eventSent := s.eventSent.Swap(true)

	pi := &livekit.ParticipantInfo{
		Identity: string(subTrack.PublisherIdentity()),
		Sid:      string(subTrack.PublisherID()),
	}
	ts.TrackSubscribed(context.Background(), pID, mediaTrack.ToProto(), pi, !eventSent)
}

func (s *trackSubscription) durationSinceStart() time.Duration {
	t := s.subStartedAt.Load()
	if t == nil {
		return 0
	}
	return time.Since(*t)
}

func (s *trackSubscription) needsSubscribe() bool {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.desired && s.subscribedTrack == nil
}

func (s *trackSubscription) needsUnsubscribe() bool {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return !s.desired && s.subscribedTrack != nil
}

func (s *trackSubscription) needsBind() bool {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.desired && s.subscribedTrack != nil && !s.bound
}
