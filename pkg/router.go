/*
Copyright 2022 The Matrix.org Foundation C.I.C.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"github.com/matrix-org/waterfall/pkg/common"
	conf "github.com/matrix-org/waterfall/pkg/conference"
	"github.com/matrix-org/waterfall/pkg/signaling"
	"github.com/sirupsen/logrus"
	"maunium.net/go/mautrix/event"
)

type Conference = common.Sender[conf.MatrixMessage]

// The top-level state of the Router.
type Router struct {
	// Matrix matrix.
	matrix *signaling.MatrixClient
	// Sinks of all conferences (all calls that are currently forwarded by this SFU).
	conferenceSinks map[string]*Conference
	// Configuration for the calls.
	config conf.Config
	// A channel to serialize all incoming events to the Router.
	channel chan RouterMessage
}

// Creates a new instance of the SFU with the given configuration.
func newRouter(matrix *signaling.MatrixClient, config conf.Config) chan<- RouterMessage {
	router := &Router{
		matrix:          matrix,
		conferenceSinks: make(map[string]*common.Sender[conf.MatrixMessage]),
		config:          config,
		channel:         make(chan RouterMessage),
	}

	// Start the main loop of the Router.
	go func() {
		for msg := range router.channel {
			switch msg := msg.(type) {
			// To-Device message received from the remote peer.
			case MatrixMessage:
				router.handleMatrixEvent(msg)
			// One of the conferences has ended.
			case ConferenceEndedMessage:
				// Remove the conference that ended from the list.
				delete(router.conferenceSinks, msg.conferenceID)
				// Process the message that was not read by the conference.
				if msg.unread != nil {
					// TODO: We must handle this message to avoid glare on session end.
					// router.handleMatrixEvent(*msg.unread)
				}
			}
		}
	}()

	return router.channel
}

// Handles incoming To-Device events that the SFU receives from clients.
func (r *Router) handleMatrixEvent(evt *event.Event) {
	// Check if `conf_id` is present in the message (right messages do have it).
	rawConferenceID, ok := evt.Content.Raw["conf_id"]
	if !ok {
		return
	}

	// Try to parse the conference ID without parsing the whole event.
	conferenceID, ok := rawConferenceID.(string)
	if !ok {
		return
	}

	logger := logrus.WithFields(logrus.Fields{
		"type":    evt.Type.Type,
		"user_id": evt.Sender.String(),
		"conf_id": conferenceID,
	})

	conference := r.conferenceSinks[conferenceID]

	// Only ToDeviceCallInvite events are allowed to create a new conference, others
	// are expected to operate on an existing conference that is running on the SFU.
	if conference == nil && evt.Type.Type != event.ToDeviceCallInvite.Type {
		if evt.Type.Type == event.ToDeviceCallInvite.Type {
			logger.Infof("creating new conference %s", conferenceID)
			conferenceSink, err := conf.StartConference(
				conferenceID,
				r.config,
				r.matrix.CreateForConference(conferenceID),
				createConferenceEndNotifier(conferenceID, r.channel),
				evt.Sender,
				evt.Content.AsCallInvite(),
			)
			if err != nil {
				logger.WithError(err).Errorf("failed to start conference %s", conferenceID)
				return
			}

			r.conferenceSinks[conferenceID] = conferenceSink
			return
		}

		logger.Warnf("ignoring %s for an unknown conference %s, ignoring", &event.ToDeviceCallInvite.Type)
		return
	}

	// A helper function to deal with messages that can't be sent due to the conference closed.
	// Not a function due to the need to capture local environment.
	sendToConference := func(eventContent conf.MessageContent) {
		// At this point the conference is not nil.
		// Let's check if the channel is still available.
		if conference.Send(conf.MatrixMessage{UserID: evt.Sender, Content: eventContent}) != nil {
			// If sending failed, then the conference is over.
			delete(r.conferenceSinks, conferenceID)
			// Since we were not able to send the message, let's re-process it now.
			// Note, we probably do not want to block here!
			// TODO: Do it better (use buffered channels or something).
			r.handleMatrixEvent(evt)
		}
	}

	switch evt.Type.Type {
	// Someone tries to participate in a call (join a call).
	case event.ToDeviceCallInvite.Type:
		// If there is an invitation sent and the conference does not exist, create one.
		sendToConference(evt.Content.AsCallInvite())
	case event.ToDeviceCallCandidates.Type:
		// Someone tries to send ICE candidates to the existing call.
		sendToConference(evt.Content.AsCallCandidates())
	case event.ToDeviceCallSelectAnswer.Type:
		// Someone informs us about them accepting our (SFU's) SDP answer for an existing call.
		sendToConference(evt.Content.AsCallSelectAnswer())
	case event.ToDeviceCallHangup.Type:
		// Someone tries to inform us about leaving an existing call.
		sendToConference(evt.Content.AsCallHangup())
	default:
		logger.Warnf("ignoring event that we must not receive: %s", evt.Type.Type)
	}
}

type RouterMessage = interface{}

type MatrixMessage = *event.Event

// Message that is sent from the conference when the conference is ended.
type ConferenceEndedMessage struct {
	// The ID of the conference that has ended.
	conferenceID string
	// A message (or messages in future) that has not been processed (if any).
	unread *conf.MatrixMessage
}

// A simple wrapper around channel that contains the ID of the conference that sent the message.
type ConferenceEndNotifier struct {
	conferenceID string
	channel      chan<- interface{}
}

// Crates a simple notifier with a conference with a given ID.
func createConferenceEndNotifier(conferenceID string, channel chan<- RouterMessage) *ConferenceEndNotifier {
	return &ConferenceEndNotifier{
		conferenceID: conferenceID,
		channel:      channel,
	}
}

// A function that a conference calls when it is ended.
func (c *ConferenceEndNotifier) Notify(unread *conf.MatrixMessage) {
	c.channel <- ConferenceEndedMessage{
		conferenceID: c.conferenceID,
		unread:       unread,
	}
}
