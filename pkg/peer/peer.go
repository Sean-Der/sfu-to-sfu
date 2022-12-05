package peer

import (
	"errors"
	"io"
	"sync"
	"time"

	"github.com/matrix-org/waterfall/pkg/common"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/sirupsen/logrus"
	"golang.org/x/exp/slices"
	"maunium.net/go/mautrix/event"
)

var (
	ErrCantCreatePeerConnection   = errors.New("can't create peer connection")
	ErrCantSetRemoteDescription   = errors.New("can't set remote description")
	ErrCantCreateAnswer           = errors.New("can't create answer")
	ErrCantSetLocalDescription    = errors.New("can't set local description")
	ErrCantCreateLocalDescription = errors.New("can't create local description")
	ErrDataChannelNotAvailable    = errors.New("data channel is not available")
	ErrDataChannelNotReady        = errors.New("data channel is not ready")
	ErrCantSubscribeToTrack       = errors.New("can't subscribe to track")
	ErrCantWriteRTCP              = errors.New("can't write RTCP")
)

// A wrapped representation of the peer connection (single peer in the call).
// The peer gets information about the things happening outside via public methods
// and informs the outside world about the things happening inside the peer by posting
// the messages to the channel.
type Peer[ID comparable] struct {
	logger         *logrus.Entry
	peerConnection *webrtc.PeerConnection
	sink           *common.MessageSink[ID, MessageContent]
	heartbeat      chan HeartBeat

	dataChannelMutex sync.Mutex
	dataChannel      *webrtc.DataChannel
}

// Instantiates a new peer with a given SDP offer and returns a peer and the SDP answer if everything is ok.
func NewPeer[ID comparable](
	sdpOffer string,
	sink *common.MessageSink[ID, MessageContent],
	logger *logrus.Entry,
	keepAliveDeadline time.Duration,
) (*Peer[ID], *webrtc.SessionDescription, error) {
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		logger.WithError(err).Error("failed to create peer connection")
		return nil, nil, ErrCantCreatePeerConnection
	}

	peer := &Peer[ID]{
		logger:         logger,
		peerConnection: peerConnection,
		sink:           sink,
		heartbeat:      make(chan HeartBeat, common.UnboundedChannelSize),
	}

	peerConnection.OnTrack(peer.onRtpTrackReceived)
	peerConnection.OnDataChannel(peer.onDataChannelReady)
	peerConnection.OnICECandidate(peer.onICECandidateGathered)
	peerConnection.OnNegotiationNeeded(peer.onNegotiationNeeded)
	peerConnection.OnICEConnectionStateChange(peer.onICEConnectionStateChanged)
	peerConnection.OnICEGatheringStateChange(peer.onICEGatheringStateChanged)
	peerConnection.OnConnectionStateChange(peer.onConnectionStateChanged)
	peerConnection.OnSignalingStateChange(peer.onSignalingStateChanged)

	if sdpAnswer, err := peer.ProcessSDPOffer(sdpOffer); err != nil {
		return nil, nil, err
	} else {
		onDeadline := func() { peer.sink.Send(LeftTheCall{event.CallHangupKeepAliveTimeout}) }
		startKeepAlive(keepAliveDeadline, peer.heartbeat, onDeadline)
		return peer, sdpAnswer, nil
	}
}

// Closes peer connection. From this moment on, no new messages will be sent from the peer.
func (p *Peer[ID]) Terminate() {
	if err := p.peerConnection.Close(); err != nil {
		p.logger.WithError(err).Error("failed to close peer connection")
	}

	// We want to seal the channel since the sender is not interested in us anymore.
	// We may want to remove this logic if/once we want to receive messages (confirmation of close or whatever)
	// from the peer that is considered closed.
	p.sink.Seal()
}

// Adds given track to our peer connection, so that it can be sent to the remote peer.
func (p *Peer[ID]) SubscribeTo(track *webrtc.TrackLocalStaticRTP) error {
	rtpSender, err := p.peerConnection.AddTrack(track)
	if err != nil {
		p.logger.WithError(err).Error("failed to add track")
		return ErrCantSubscribeToTrack
	}

	// Read incoming RTCP packets
	// Before these packets are returned they are processed by interceptors. For things
	// like NACK this needs to be called.
	go func() {
		for {
			packets, _, err := rtpSender.ReadRTCP()
			if err != nil {
				if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, io.EOF) {
					return
				}

				p.logger.WithError(err).Warn("failed to read RTCP on track")
			}

			p.sink.Send(RTCPReceived{Packets: packets, TrackID: track.ID(), StreamID: track.StreamID()})
		}
	}()

	return nil
}

func (p *Peer[ID]) WriteRTCP(packets []rtcp.Packet, streamID string, trackID string, lastPLITimestamp time.Time) error {
	const minimalPLIInterval = time.Millisecond * 500

	packetsToSend := []rtcp.Packet{}
	var mediaSSRC uint32
	receivers := p.peerConnection.GetReceivers()
	receiverIndex := slices.IndexFunc(receivers, func(receiver *webrtc.RTPReceiver) bool {
		return receiver.Track().ID() == trackID && receiver.Track().StreamID() == streamID
	})

	if receiverIndex == -1 {
		p.logger.Error("failed to find track to write RTCP on")
		return ErrCantWriteRTCP
	} else {
		mediaSSRC = uint32(receivers[receiverIndex].Track().SSRC())
	}

	for _, packet := range packets {
		switch typedPacket := packet.(type) {
		// We mung the packets here, so that the SSRCs match what the
		// receiver expects:
		// The media SSRC is the SSRC of the media about which the packet is
		// reporting; therefore, we mung it to be the SSRC of the publishing
		// participant's track. Without this, it would be SSRC of the SFU's
		// track which isn't right
		case *rtcp.PictureLossIndication:
			// Since we sometimes spam the sender with PLIs, make sure we don't send
			// them way too often
			if time.Now().UnixNano()-lastPLITimestamp.UnixNano() < minimalPLIInterval.Nanoseconds() {
				continue
			}

			p.sink.Send(PLISent{Timestamp: time.Now(), StreamID: streamID, TrackID: trackID})

			typedPacket.MediaSSRC = mediaSSRC
			packetsToSend = append(packetsToSend, typedPacket)
		case *rtcp.FullIntraRequest:
			typedPacket.MediaSSRC = mediaSSRC
			packetsToSend = append(packetsToSend, typedPacket)
		}

		packetsToSend = append(packetsToSend, packet)
	}

	if len(packetsToSend) != 0 {
		if err := p.peerConnection.WriteRTCP(packetsToSend); err != nil {
			if !errors.Is(err, io.ErrClosedPipe) {
				p.logger.WithError(err).Error("failed to write RTCP on track")
				return err
			}
		}
	}

	return nil
}

// Unsubscribes from the given list of tracks.
func (p *Peer[ID]) UnsubscribeFrom(tracks []*webrtc.TrackLocalStaticRTP) {
	// That's unfortunately an O(m*n) operation, but we don't expect the number of tracks to be big.
	for _, sender := range p.peerConnection.GetSenders() {
		currentTrack := sender.Track()
		if currentTrack == nil {
			return
		}

		for _, trackToUnsubscribe := range tracks {
			presentTrackID, presentStreamID := currentTrack.ID(), currentTrack.StreamID()
			trackID, streamID := trackToUnsubscribe.ID(), trackToUnsubscribe.StreamID()
			if presentTrackID == trackID && presentStreamID == streamID {
				if err := p.peerConnection.RemoveTrack(sender); err != nil {
					p.logger.WithError(err).Error("failed to remove track")
				}
			}
		}
	}
}

// Tries to send the given message to the remote counterpart of our peer.
func (p *Peer[ID]) SendOverDataChannel(json string) error {
	p.dataChannelMutex.Lock()
	defer p.dataChannelMutex.Unlock()

	if p.dataChannel == nil {
		p.logger.Error("can't send data over data channel: data channel is not ready")
		return ErrDataChannelNotAvailable
	}

	if p.dataChannel.ReadyState() != webrtc.DataChannelStateOpen {
		p.logger.Error("can't send data over data channel: data channel is not open")
		return ErrDataChannelNotReady
	}

	if err := p.dataChannel.SendText(json); err != nil {
		p.logger.WithError(err).Error("failed to send data over data channel")
	}

	return nil
}

// Processes the remote ICE candidates.
func (p *Peer[ID]) ProcessNewRemoteCandidates(candidates []webrtc.ICECandidateInit) {
	for _, candidate := range candidates {
		if err := p.peerConnection.AddICECandidate(candidate); err != nil {
			p.logger.WithError(err).Error("failed to add ICE candidate")
		}
	}
}

// Processes the SDP answer received from the remote peer.
func (p *Peer[ID]) ProcessSDPAnswer(sdpAnswer string) error {
	err := p.peerConnection.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdpAnswer,
	})
	if err != nil {
		p.logger.WithError(err).Error("failed to set remote description")
		return ErrCantSetRemoteDescription
	}

	return nil
}

// Applies the sdp offer received from the remote peer and generates an SDP answer.
func (p *Peer[ID]) ProcessSDPOffer(sdpOffer string) (*webrtc.SessionDescription, error) {
	err := p.peerConnection.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdpOffer,
	})
	if err != nil {
		p.logger.WithError(err).Error("failed to set remote description")
		return nil, ErrCantSetRemoteDescription
	}

	answer, err := p.peerConnection.CreateAnswer(nil)
	if err != nil {
		p.logger.WithError(err).Error("failed to create answer")
		return nil, ErrCantCreateAnswer
	}

	if err := p.peerConnection.SetLocalDescription(answer); err != nil {
		p.logger.WithError(err).Error("failed to set local description")
		return nil, ErrCantSetLocalDescription
	}

	return &answer, nil
}

// New heartbeat received (keep-alive message that is periodically sent by the remote peer).
// We need to update the last heartbeat time. If the peer is not active for too long, we will
// consider peer's connection as stalled and will close it.
func (p *Peer[ID]) ProcessHeartbeat() {
	p.heartbeat <- HeartBeat{}
}
