package track

import (
	"fmt"
	"time"

	"github.com/matrix-org/waterfall/pkg/conference/publisher"
	"github.com/matrix-org/waterfall/pkg/telemetry"
	"github.com/matrix-org/waterfall/pkg/webrtc_ext"
	"github.com/pion/webrtc/v3"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
)

type trackOwner[SubscriberID comparable] struct {
	owner           SubscriberID
	requestKeyFrame func(track *webrtc.TrackRemote) error
}

type audioTrack struct {
	// The sink of this audio track packets.
	outputTrack *webrtc.TrackLocalStaticRTP
}

type videoTrack struct {
	// Publishers of each video layer.
	publishers map[webrtc_ext.SimulcastLayer]*publisher.Publisher
}

// Forward audio packets from the source track to the destination track.
func forward(sender *webrtc.TrackRemote, receiver *webrtc.TrackLocalStaticRTP, stop <-chan struct{}) error {
	for {
		// Read the data from the remote track.
		packet, _, readErr := sender.ReadRTP()
		if readErr != nil {
			return readErr
		}

		// Write the data to the local track.
		if writeErr := receiver.WriteRTP(packet); writeErr != nil {
			return writeErr
		}

		// Check if we need to stop processing packets.
		select {
		case <-stop:
			return nil
		default:
		}
	}
}

func (p *PublishedTrack[SubscriberID]) addVideoPublisher(track *webrtc.TrackRemote) {
	// Detect simulcast layer of a publisher and create loggers and scoped telemetry.
	simulcast := webrtc_ext.RIDToSimulcastLayer(track.RID())
	pubLogger := p.logger.WithField("layer", simulcast.String())
	pubTelemetry := p.telemetry.CreateChild("layer", attribute.String("layer", simulcast.String()))

	// Create a new publisher for the track.
	pub, pubCh := publisher.NewPublisher(
		&publisher.RemoteTrack{track},
		p.stopPublishers,
		2*time.Second, // We consider publisher as stalled if there are no packets within 2 seconds.
		pubLogger,
	)
	p.video.publishers[simulcast] = pub

	// Observe the status of the publisher.
	p.activePublishers.Add(1)
	go p.processPublisherEvents(pub, pubCh, simulcast, pubLogger, pubTelemetry)
}

// Processes the events from a single publisher, i.e. a single track, i.e. a single layer.
func (p *PublishedTrack[SubscriberID]) processPublisherEvents(
	pub *publisher.Publisher,
	pubChannel <-chan publisher.Status,
	pubLayer webrtc_ext.SimulcastLayer,
	pubLogger *logrus.Entry,
	pubTelemetry *telemetry.Telemetry,
) {
	// Once this go-routine is done, inform that this publisher is stopped.
	defer p.activePublishers.Done()
	defer pubTelemetry.End()

	// Observe publisher's status events.
	for status := range pubChannel {
		switch status {
		// Publisher is not active (no packets received for a while).
		case publisher.StatusStalled:
			p.mutex.Lock()
			defer p.mutex.Unlock()

			// Let's check if we're muted. If we are, it's ok to not receive packets.
			if p.metadata.Muted {
				pubLogger.Info("Publisher is stalled but we're muted, ignoring")
				pubTelemetry.AddEvent("muted")
				continue
			}

			// Otherwise, remove all subscriptions and switch them to the lowest layer if available.
			// We assume that the lowest layer is the latest to fail (normally, lowest layer always
			// receive packets even if other layers are stalled).

			subscriptionsMap := p.getSubscriptionByLayer(pubLayer)
			subscriptions := []publisher.Subscription{}
			for _, subscription := range subscriptionsMap {
				subscriptions = append(subscriptions, subscription.subscription)
			}

			pub.RemoveSubscription(subscriptions...)

			lowLayer := p.video.publishers[webrtc_ext.SimulcastLayerLow]
			if lowLayer != nil {
				pubLogger.Info("Publisher is stalled, switching to the lowest layer")
				pubTelemetry.AddEvent("stalled, so subscriptions switched to the low layer")
				lowLayer.AddSubscription(subscriptions...)
				for _, subscription := range subscriptionsMap {
					subscription.currentLayer = webrtc_ext.SimulcastLayerLow
				}
				continue
			}

			// Otherwise, we have no other layer to switch to. Bummer.
			pubLogger.Warn("Publisher is stalled and we have no other layer to switch to")
			pubTelemetry.Fail(fmt.Errorf("stalled"))
			for _, subscription := range subscriptionsMap {
				subscription.currentLayer = webrtc_ext.SimulcastLayerNone
			}

		// Publisher is active again (new packets received).
		case publisher.StatusRecovered:
			p.mutex.Lock()
			defer p.mutex.Unlock()

			pubLogger.Info("Publisher is recovered")
			pubTelemetry.AddEvent("recovered")

			// Iterate over active subscriptions that don't have any active publisher
			// and assign them to this publisher.
			for _, subscription := range p.subscriptions {
				if subscription.currentLayer == webrtc_ext.SimulcastLayerNone {
					subscription.currentLayer = pubLayer
					pub.AddSubscription(subscription.subscription)
				}
			}
		}
	}

	pubTelemetry.AddEvent("stopped, removing dependent subscriptions")

	// If we got there, then the publisher is stopped.
	p.mutex.Lock()
	defer p.mutex.Unlock()

	// Remove the publisher once it's gone.
	delete(p.video.publishers, pubLayer)

	// Now iterate over all subscriptions and find those that are now lost due to the publisher being away.
	// It seems like normally when a single track or layer is gone, it's due to failure, so we don't switch
	// to different layers here, but instead just remove the dependent subscriptions.
	for subID, sub := range p.subscriptions {
		if sub.currentLayer == pubLayer {
			sub.subscription.Unsubscribe()
			delete(p.subscriptions, subID)
		}
	}
}

func (p *PublishedTrack[SubscriberID]) isClosed() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *PublishedTrack[SubscriberID]) getSubscriptionByLayer(
	layer webrtc_ext.SimulcastLayer,
) map[SubscriberID]*trackSubscription {
	subscriptions := map[SubscriberID]*trackSubscription{}
	for subID, sub := range p.subscriptions {
		if sub.currentLayer == layer {
			subscriptions[subID] = sub
		}
	}
	return subscriptions
}