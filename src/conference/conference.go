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

package conference

import (
	"github.com/matrix-org/waterfall/src/peer"
	"github.com/matrix-org/waterfall/src/signaling"
	"github.com/pion/webrtc/v3"
	"github.com/sirupsen/logrus"
	"maunium.net/go/mautrix/event"
)

type Conference struct {
	id               string
	config           Config
	signaling        signaling.MatrixSignaling
	participants     map[peer.ID]*Participant
	peerEventsStream chan peer.Message
	logger           *logrus.Entry
}

func NewConference(confID string, config Config, signaling signaling.MatrixSignaling) *Conference {
	conference := &Conference{
		id:               confID,
		config:           config,
		signaling:        signaling,
		participants:     make(map[peer.ID]*Participant),
		peerEventsStream: make(chan peer.Message),
		logger:           logrus.WithFields(logrus.Fields{"conf_id": confID}),
	}

	// Start conference "main loop".
	go conference.processMessages()
	return conference
}

// New participant tries to join the conference.
func (c *Conference) OnNewParticipant(participantID peer.ID, inviteEvent *event.CallInviteEventContent) {
	// As per MSC3401, when the `session_id` field changes from an incoming `m.call.member` event,
	// any existing calls from this device in this call should be terminated.
	// TODO: Implement this.
	for id, participant := range c.participants {
		if id.DeviceID == inviteEvent.DeviceID {
			if participant.remoteSessionID == inviteEvent.SenderSessionID {
				c.logger.WithFields(logrus.Fields{
					"device_id":  inviteEvent.DeviceID,
					"session_id": inviteEvent.SenderSessionID,
				}).Errorf("Found existing participant with equal DeviceID and SessionID")
				return
			} else {
				participant.peer.Terminate()
			}
		}
	}

	peer, sdpOffer, err := peer.NewPeer(participantID, c.id, inviteEvent.Offer.SDP, c.peerEventsStream)
	if err != nil {
		c.logger.WithError(err).Errorf("Failed to create new peer")
		return
	}

	participant := &Participant{
		id:              participantID,
		peer:            peer,
		remoteSessionID: inviteEvent.SenderSessionID,
		streamMetadata:  inviteEvent.SDPStreamMetadata,
		publishedTracks: make(map[event.SFUTrackDescription]*webrtc.TrackLocalStaticRTP),
	}

	c.participants[participantID] = participant

	recipient := participant.asMatrixRecipient()
	streamMetadata := c.getStreamsMetadata(participantID)
	c.signaling.SendSDPAnswer(recipient, streamMetadata, sdpOffer.SDP)
}

func (c *Conference) OnCandidates(peerID peer.ID, candidatesEvent *event.CallCandidatesEventContent) {
	if participant := c.getParticipant(peerID, nil); participant != nil {
		// Convert the candidates to the WebRTC format.
		candidates := make([]webrtc.ICECandidateInit, len(candidatesEvent.Candidates))
		for i, candidate := range candidatesEvent.Candidates {
			SDPMLineIndex := uint16(candidate.SDPMLineIndex)
			candidates[i] = webrtc.ICECandidateInit{
				Candidate:     candidate.Candidate,
				SDPMid:        &candidate.SDPMID,
				SDPMLineIndex: &SDPMLineIndex,
			}
		}

		participant.peer.AddICECandidates(candidates)
	}
}

func (c *Conference) OnSelectAnswer(peerID peer.ID, selectAnswerEvent *event.CallSelectAnswerEventContent) {
	if participant := c.getParticipant(peerID, nil); participant != nil {
		if selectAnswerEvent.SelectedPartyID != peerID.DeviceID.String() {
			c.logger.WithFields(logrus.Fields{
				"device_id": selectAnswerEvent.SelectedPartyID,
			}).Errorf("Call was answered on a different device, kicking this peer")
			participant.peer.Terminate()
		}
	}
}

func (c *Conference) OnHangup(peerID peer.ID, hangupEvent *event.CallHangupEventContent) {
	if participant := c.getParticipant(peerID, nil); participant != nil {
		participant.peer.Terminate()
	}
}
