package subscription

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/matrix-org/waterfall/pkg/common"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/sirupsen/logrus"
)

type ConnectionController interface {
	Subscribe(track *webrtc.TrackLocalStaticRTP) (*webrtc.RTPSender, error)
	Unsubscribe(sender *webrtc.RTPSender) error
	RequestKeyFrame(track common.TrackInfo)
}

type Subscription struct {
	rtpSender  *webrtc.RTPSender
	rtpTrack   *webrtc.TrackLocalStaticRTP
	info       common.TrackInfo
	connection ConnectionController
	watchdog   *common.WatchdogChannel
	logger     *logrus.Entry
}

func NewSubscription(
	info common.TrackInfo,
	connection ConnectionController,
	logger *logrus.Entry,
) (*Subscription, error) {
	// Set the RID if any (would be "" if no simulcast is used).
	setRid := webrtc.WithRTPStreamID(common.SimulcastLayerToRID(info.Layer))

	// Create a new track.
	rtpTrack, err := webrtc.NewTrackLocalStaticRTP(info.Codec, info.TrackID, info.StreamID, setRid)
	if err != nil {
		return nil, fmt.Errorf("Failed to create track: %s", err)
	}

	rtpSender, err := connection.Subscribe(rtpTrack)
	if err != nil {
		return nil, fmt.Errorf("Failed to add track: %s", err)
	}

	// Configure watchdog for the subscription so that we know when we don't receive any new frames.
	watchdogConfig := common.WatchdogConfig{
		Timeout: 2 * time.Second,
		OnTimeout: func() {
			logger.Warnf("No RTP on subscription for %s (%s)", info.TrackID, info.Layer)
			connection.RequestKeyFrame(info)
		},
	}

	// Start a watchdog for the subscription and create a subsription.
	watchdog := common.StartWatchdog(watchdogConfig)
	subscription := &Subscription{rtpSender, rtpTrack, info, connection, watchdog, logger}

	// Start reading and forwarding RTCP packets.
	go subscription.readRTCP()

	return subscription, nil
}

func (s *Subscription) Unsubscribe() error {
	s.watchdog.Close()
	return s.connection.Unsubscribe(s.rtpSender)
}

func (s *Subscription) WriteRTP(packet *rtp.Packet) error {
	if !s.watchdog.Notify() {
		s.logger.Errorf("Subscription to %s is closed", s.info.TrackID)
	}

	return s.rtpTrack.WriteRTP(packet)
}

func (s *Subscription) TrackInfo() common.TrackInfo {
	return s.info
}

// Read incoming RTCP packets. Before these packets are returned they are processed by interceptors.
func (s *Subscription) readRTCP() {
	for {
		packets, _, err := s.rtpSender.ReadRTCP()
		if err != nil {
			if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, io.EOF) {
				s.logger.Warnf("failed to read RTCP on track: %s", err)
				s.watchdog.Close()
				return
			}
		}

		// We only want to inform others about PLIs and FIRs. We skip the rest of the packets for now.
		for _, packet := range packets {
			switch packet.(type) {
			// For simplicity we assume that any of the key frame requests is just a key frame request.
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				s.connection.RequestKeyFrame(s.info)
			}
		}
	}
}
