package dtsync

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	rate "golang.org/x/time/rate"

	dt "github.com/filecoin-project/go-data-transfer"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	"github.com/ipfs/go-graphsync"
	logging "github.com/ipfs/go-log/v2"
	"github.com/ipld/go-ipld-prime"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/peer"
)

var log = logging.Logger("go-legs-dtsync")

const hitRateLimitErrStr = "hit rate limit"

type inProgressSyncKey struct {
	c    cid.Cid
	peer peer.ID
}

// Sync provides sync functionality for use with all datatransfer syncs.
type Sync struct {
	dtManager   dt.Manager
	dtClose     dtCloseFunc
	host        host.Host
	unsubEvents dt.Unsubscribe
	unregHook   graphsync.UnregisterHookFunc

	// Map of CID of in-progress sync to sync done channel.
	syncDoneChans map[inProgressSyncKey]chan<- error
	syncDoneMutex sync.Mutex

	limiterFor rateLimiterFor

	// overrideRateLimiterFor let's a specific sync define its own rate limiter
	overrideRateLimiterFor   map[peer.ID]*rate.Limiter
	overrideRateLimiterForMu sync.RWMutex

	// isRetryingDueToRateLimit keeps track of if an existing sync is retrying due
	// to a previous failed rate limit.  It is used to prevent the caller's block
	// hook from being called multiple times while we retry due to rate limit.
	//
	// The value represents the last block that it did not call the caller's block
	// hook on.
	isRetryingDueToRateLimit sync.Map // concurrent version of map[peer.ID]cid.Cid
}

// wrapRateLimiterFor wraps a rateLimiterFor function with override semantics so
// that a manual Sync can use a different rate limiter
func (s *Sync) wrapRateLimiterFor(limiterFor rateLimiterFor) rateLimiterFor {
	return func(p peer.ID) *rate.Limiter {
		s.overrideRateLimiterForMu.RLock()
		defer s.overrideRateLimiterForMu.RUnlock()
		if s.overrideRateLimiterFor[p] != nil {
			return s.overrideRateLimiterFor[p]
		}

		return limiterFor(p)
	}
}

// NewSyncWithDT creates a new Sync with a datatransfer.Manager provided by the
// caller.
func NewSyncWithDT(host host.Host, dtManager dt.Manager, gs graphsync.GraphExchange, blockHook func(peer.ID, cid.Cid), limiterFor rateLimiterFor) (*Sync, error) {
	registerVoucher(dtManager)
	s := &Sync{
		host:                   host,
		dtManager:              dtManager,
		overrideRateLimiterFor: make(map[peer.ID]*rate.Limiter),
		limiterFor:             limiterFor,
	}

	if blockHook != nil {
		s.unregHook = gs.RegisterIncomingBlockHook(s.addRateLimiting(addIncomingBlockHook(nil, blockHook), s.wrapRateLimiterFor(limiterFor), gs))
	}

	s.unsubEvents = dtManager.SubscribeToEvents(s.onEvent)
	return s, nil
}

// purposely a type alias
type rateLimiterFor = func(publisher peer.ID) *rate.Limiter

// NewSync creates a new Sync with its own datatransfer.Manager.
func NewSync(host host.Host, ds datastore.Batching, lsys ipld.LinkSystem, blockHook func(peer.ID, cid.Cid), limiterFor rateLimiterFor) (*Sync, error) {
	dtManager, gs, dtClose, err := makeDataTransfer(host, ds, lsys)
	if err != nil {
		return nil, err
	}

	s := &Sync{
		host:                   host,
		dtManager:              dtManager,
		dtClose:                dtClose,
		overrideRateLimiterFor: make(map[peer.ID]*rate.Limiter),
		limiterFor:             limiterFor,
	}

	if blockHook != nil {
		s.unregHook = gs.RegisterIncomingBlockHook(s.addRateLimiting(addIncomingBlockHook(nil, blockHook), s.wrapRateLimiterFor(limiterFor), gs))
	}

	s.unsubEvents = dtManager.SubscribeToEvents(s.onEvent)
	return s, nil
}

func (s *Sync) addRateLimiting(bFn graphsync.OnIncomingBlockHook, rateLimiter rateLimiterFor, gs graphsync.GraphExchange) graphsync.OnIncomingBlockHook {
	return func(p peer.ID, responseData graphsync.ResponseData, blockData graphsync.BlockData, hookActions graphsync.IncomingBlockHookActions) {
		isLocalBlock := blockData.BlockSizeOnWire() == 0

		if !isLocalBlock {
			limiter := rateLimiter(p)
			if !limiter.Allow() {
				s.isRetryingDueToRateLimit.Store(p, blockData.Link().(cidlink.Link).Cid)
				hookActions.TerminateWithError(errors.New(hitRateLimitErrStr))
				return
			}
		}

		lastFailedBlock, isRetryingDueToRateLimit := s.isRetryingDueToRateLimit.Load(p)
		if isRetryingDueToRateLimit && lastFailedBlock == blockData.Link().(cidlink.Link).Cid {
			s.isRetryingDueToRateLimit.Delete(p)
		} else if isRetryingDueToRateLimit {
			// We're in a retry loop due to rate limiting and we haven't seen the
			// block that we stopped at before, so we won't call the wrapped block
			// hook. This is because we already called it in a previous iteration of this sync.
			return
		}

		if bFn != nil {
			bFn(p, responseData, blockData, hookActions)
		}
	}
}

func addIncomingBlockHook(bFn graphsync.OnIncomingBlockHook, blockHook func(peer.ID, cid.Cid)) graphsync.OnIncomingBlockHook {
	return func(p peer.ID, responseData graphsync.ResponseData, blockData graphsync.BlockData, hookActions graphsync.IncomingBlockHookActions) {
		blockHook(p, blockData.Link().(cidlink.Link).Cid)
		if bFn != nil {
			bFn(p, responseData, blockData, hookActions)
		}
	}
}

// Close unregisters datatransfer event notification. If this Sync owns the
// datatransfer.Manager then the Manager is stopped.
func (s *Sync) Close() error {
	s.unsubEvents()
	if s.unregHook != nil {
		s.unregHook()
	}

	var err error
	if s.dtClose != nil {
		err = s.dtClose()
	}

	// Dismiss any handlers waiting completion of sync.
	s.syncDoneMutex.Lock()
	if len(s.syncDoneChans) != 0 {
		log.Warnf("Closing datatransfer sync with %d syncs in progress", len(s.syncDoneChans))
	}
	for _, ch := range s.syncDoneChans {
		ch <- errors.New("sync closed")
		close(ch)
	}
	s.syncDoneChans = nil
	s.syncDoneMutex.Unlock()

	return err
}

// NewSyncer creates a new Syncer to use for a single sync operation against a peer.
func (s *Sync) NewSyncer(peerID peer.ID, topicName string, rateLimiter *rate.Limiter) *Syncer {
	return &Syncer{
		peerID:      peerID,
		sync:        s,
		topicName:   topicName,
		rateLimiter: s.limiterFor(peerID),
	}
}

// notifyOnSyncDone returns a channel that sync done notification is sent on.
func (s *Sync) notifyOnSyncDone(k inProgressSyncKey) <-chan error {
	syncDone := make(chan error, 1)

	s.syncDoneMutex.Lock()
	defer s.syncDoneMutex.Unlock()

	if s.syncDoneChans == nil {
		s.syncDoneChans = make(map[inProgressSyncKey]chan<- error)
	}
	s.syncDoneChans[k] = syncDone

	return syncDone
}

// signalSyncDone removes and closes the channel when the pending sync has
// completed.  Returns true if a channel was found.
func (s *Sync) signalSyncDone(k inProgressSyncKey, err error) bool {
	s.syncDoneMutex.Lock()
	defer s.syncDoneMutex.Unlock()

	syncDone, ok := s.syncDoneChans[k]
	if !ok {
		return false
	}
	if len(s.syncDoneChans) == 1 {
		s.syncDoneChans = nil
	} else {
		delete(s.syncDoneChans, k)
	}

	if err != nil {
		syncDone <- err
	}
	close(syncDone)
	return true
}

type rateLimitErr struct {
	msg string
}

func (e rateLimitErr) Error() string { return e.msg }

// onEvent is called by the datatransfer manager to send events.
func (s *Sync) onEvent(event dt.Event, channelState dt.ChannelState) {
	var err error
	switch channelState.Status() {
	case dt.Completed:
		// Tell the waiting handler that the sync has finished successfully.
		log.Infow("datatransfer completed successfully", "cid", channelState.BaseCID(), "peer", channelState.OtherPeer())
	case dt.Cancelled:
		// The request was canceled; inform waiting handler.
		err = fmt.Errorf("datatransfer cancelled")
		log.Warnw(err.Error(), "cid", channelState.BaseCID(), "peer", channelState.OtherPeer(), "message", channelState.Message())
	case dt.Failed:
		// Communicate the error back to the waiting handler.
		msg := channelState.Message()
		if strings.Contains(msg, hitRateLimitErrStr) {
			err = rateLimitErr{msg}
		} else {
			err = fmt.Errorf("datatransfer failed: %s", msg)
		}

		log.Errorw(err.Error(), "cid", channelState.BaseCID(), "peer", channelState.OtherPeer(), "message", msg)

		if strings.HasSuffix(msg, "content not found") {
			err = errors.New(err.Error() + ": content not found")
		}
	default:
		// Ignore non-terminal channel states.
		return
	}

	// Send the FinishTransfer signal to the handler.  This will allow its
	// handle goroutine to distribute the update and exit.
	//
	// It is not necessary to return the channelState CID, since we already
	// know it is the correct on since it was used to look up this syncDone
	// channel.
	if !s.signalSyncDone(inProgressSyncKey{channelState.BaseCID(), channelState.OtherPeer()}, err) {
		log.Errorw("Could not find channel for completed transfer notice", "cid", channelState.BaseCID())
		return
	}
}
