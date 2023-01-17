package peer

import (
	"github.com/matrix-org/waterfall/pkg/common"
	"github.com/pion/webrtc/v3"
	"maunium.net/go/mautrix/event"
)

// A callback that is called once we receive first RTP packets from a track, i.e.
// we call this function each time a new track is received.
func (p *Peer[ID]) onRtpTrackReceived(remoteTrack *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	// Construct a new track info assuming that there is no simulcast.
	trackInfo := common.TrackInfoFromTrack(remoteTrack)

	switch trackInfo.Kind {
	case webrtc.RTPCodecTypeVideo:
		p.handleNewVideoTrack(trackInfo, remoteTrack, receiver)
	case webrtc.RTPCodecTypeAudio:
		p.handleNewAudioTrack(trackInfo, remoteTrack, receiver)
	}
}

// A callback that is called once we receive an ICE candidate for this peer connection.
func (p *Peer[ID]) onICECandidateGathered(candidate *webrtc.ICECandidate) {
	if candidate == nil {
		p.logger.Info("ICE candidate gathering finished")
		p.sink.Send(ICEGatheringComplete{})
		return
	}

	p.logger.WithField("candidate", candidate).Debug("ICE candidate gathered")
	p.sink.Send(NewICECandidate{Candidate: candidate})
}

// A callback that is called once we receive an ICE connection state change for this peer connection.
func (p *Peer[ID]) onNegotiationNeeded() {
	p.logger.Debug("negotiation needed")
	offer, err := p.peerConnection.CreateOffer(nil)
	if err != nil {
		p.logger.WithError(err).Error("failed to create offer")
		return
	}

	if err := p.peerConnection.SetLocalDescription(offer); err != nil {
		p.logger.WithError(err).Error("failed to set local description")
		return
	}

	p.sink.Send(RenegotiationRequired{Offer: &offer})
}

// A callback that is called once we receive an ICE connection state change for this peer connection.
func (p *Peer[ID]) onICEConnectionStateChanged(state webrtc.ICEConnectionState) {
	p.logger.Infof("ICE connection state changed: %v", state)

	switch state {
	case webrtc.ICEConnectionStateFailed, webrtc.ICEConnectionStateDisconnected:
		// TODO: Ask Simon if we should do it here as in the previous implementation.
		//       Ideally we want to perform an ICE restart here.
		// p.notify <- PeerLeftTheCall{sender: p.data}
	case webrtc.ICEConnectionStateCompleted, webrtc.ICEConnectionStateConnected:
		// FIXME: Start keep-alive timer over the data channel to check the connecitons that hanged.
		// p.notify <- PeerJoinedTheCall{sender: p.data}
	}
}

func (p *Peer[ID]) onICEGatheringStateChanged(state webrtc.ICEGathererState) {
	p.logger.Debugf("ICE gathering state changed: %v", state)
}

func (p *Peer[ID]) onSignalingStateChanged(state webrtc.SignalingState) {
	p.logger.Debugf("signaling state changed: %v", state)
}

func (p *Peer[ID]) onConnectionStateChanged(state webrtc.PeerConnectionState) {
	p.logger.Infof("Connection state changed: %v", state)

	switch state {
	case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateClosed:
		p.sink.Send(LeftTheCall{event.CallHangupUserHangup})
	case webrtc.PeerConnectionStateConnected:
		p.sink.Send(JoinedTheCall{})
	}
}

// A callback that is called once the data channel is ready to be used.
func (p *Peer[ID]) onDataChannelReady(dc *webrtc.DataChannel) {
	if dataChannel := p.state.GetDataChannel(); dataChannel != nil {
		p.logger.Error("Data channel already exists")
		dataChannel.Close()
		p.state.SetDataChannel(nil)
		return
	}

	p.state.SetDataChannel(dc)
	p.logger.WithField("label", dc.Label()).Debug("Data channel ready")

	dc.OnOpen(func() {
		p.logger.Debug("Data channel opened")
		p.sink.Send(DataChannelAvailable{})
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if msg.IsString {
			p.sink.Send(DataChannelMessage{Message: string(msg.Data)})
		} else {
			p.logger.Warn("Data channel message is not a string, ignoring")
		}
	})

	dc.OnError(func(err error) {
		p.logger.WithError(err).Error("Data channel error")
	})

	dc.OnClose(func() {
		p.logger.Info("Data channel closed")
	})
}
