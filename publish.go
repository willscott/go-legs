package legs

import (
	"context"

	dt "github.com/filecoin-project/go-data-transfer"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	"github.com/ipld/go-ipld-prime"
	"github.com/libp2p/go-libp2p-core/host"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
)

type legPublisher struct {
	topic   *pubsub.Topic
	onClose func() error
}

// NewPublisher creates a new legs publisher
func NewPublisher(ctx context.Context,
	host host.Host,
	ds datastore.Batching,
	lsys ipld.LinkSystem,
	topic string) (LegPublisher, error) {
	ss, err := newSimpleSetup(ctx, host, ds, lsys, topic)
	if err != nil {
		return nil, err
	}
	return &legPublisher{ss.t, ss.onClose}, nil
}

// NewPublisherFromExisting instantiates go-legs publishing on an existing
// data transfer instance
func NewPublisherFromExisting(ctx context.Context,
	dt dt.Manager,
	host host.Host,
	topic string,
	lsys ipld.LinkSystem) (LegPublisher, error) {
	t, err := makePubsub(ctx, host, topic)
	if err != nil {
		return nil, err
	}
	err = configureDataTransferForLegs(ctx, dt, lsys)
	if err != nil {
		return nil, err
	}
	return &legPublisher{t, t.Close}, nil
}

func (lp *legPublisher) UpdateRoot(ctx context.Context, c cid.Cid, opts ...pubsub.PubOpt) error {
	// By default, we block until we have one other peer in the topic.
	// This ensures UpdateRoot never succeeds when there aren't any peers,
	// in which case performing the Publish would probably be pointless.
	// The user can override this default by supplying their own WithReadiness.
	opts = append([]pubsub.PubOpt{pubsub.WithReadiness(pubsub.MinTopicSize(1))}, opts...)

	log.Debugf("Published CID in pubsub channel: %s", c)
	return lp.topic.Publish(ctx, c.Bytes(), opts...)
}

func (lp *legPublisher) Close() error {
	return lp.onClose()
}
