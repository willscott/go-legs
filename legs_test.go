package legs_test

import (
	"context"
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"
	gsimpl "github.com/ipfs/go-graphsync/impl"
	gsnet "github.com/ipfs/go-graphsync/network"

	// dagjson codec registered for encoding
	datatransfer "github.com/filecoin-project/go-data-transfer/impl"
	dtnetwork "github.com/filecoin-project/go-data-transfer/network"
	gstransport "github.com/filecoin-project/go-data-transfer/transport/graphsync"
	"github.com/filecoin-project/go-legs"
	"github.com/filecoin-project/go-legs/dtsync"
	"github.com/filecoin-project/go-legs/test"
	_ "github.com/ipld/go-ipld-prime/codec/dagcbor"
	_ "github.com/ipld/go-ipld-prime/codec/dagjson"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	basicnode "github.com/ipld/go-ipld-prime/node/basic"
	"github.com/libp2p/go-libp2p-core/host"
)

func initPubSub(t *testing.T, srcStore, dstStore datastore.Batching) (host.Host, host.Host, legs.LegPublisher, legs.LegSubscriber) {
	srcHost := test.MkTestHost()
	srcLnkS := test.MkLinkSystem(srcStore)
	lp, err := dtsync.NewPublisher(context.Background(), srcHost, srcStore, srcLnkS, testTopic)
	if err != nil {
		t.Fatal(err)
	}

	dstHost := test.MkTestHost()
	srcHost.Peerstore().AddAddrs(dstHost.ID(), dstHost.Addrs(), time.Hour)
	dstHost.Peerstore().AddAddrs(srcHost.ID(), srcHost.Addrs(), time.Hour)
	dstLnkS := test.MkLinkSystem(dstStore)
	ls, err := dtsync.NewSubscriber(context.Background(), dstHost, dstStore, dstLnkS, testTopic, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := srcHost.Connect(context.Background(), dstHost.Peerstore().PeerInfo(dstHost.ID())); err != nil {
		t.Fatal(err)
	}

	return srcHost, dstHost, lp, ls
}

func TestRoundTrip(t *testing.T) {
	// Init legs publisher and subscriber
	srcStore := dssync.MutexWrap(datastore.NewMapDatastore())
	dstStore := dssync.MutexWrap(datastore.NewMapDatastore())
	_, _, lp, ls := initPubSub(t, srcStore, dstStore)

	watcher, cncl := ls.OnChange()

	// Update root with item
	itm := basicnode.NewString("hello world")
	lnk, err := test.Store(srcStore, itm)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		cncl()
		lp.Close()
		ls.Close()
	})

	// per https://github.com/libp2p/go-libp2p-pubsub/blob/e6ad80cf4782fca31f46e3a8ba8d1a450d562f49/gossipsub_test.go#L103
	// we don't seem to have a way to manually trigger needed gossip-sub heartbeats for mesh establishment.
	time.Sleep(5 * time.Second)

	if err := lp.UpdateRoot(context.Background(), lnk.(cidlink.Link).Cid); err != nil {
		t.Fatal(err)
	}

	select {
	case <-time.After(time.Second * 5):
		t.Fatal("timed out waiting for sync to propogate")
	case downstream := <-watcher:
		if !downstream.Equals(lnk.(cidlink.Link).Cid) {
			t.Fatalf("sync'd sid unexpected %s vs %s", downstream, lnk)
		}
		if _, err := dstStore.Get(datastore.NewKey(downstream.String())); err != nil {
			t.Fatalf("data not in receiver store: %v", err)
		}
	}
}

func TestRoundTripExistingDataTransfer(t *testing.T) {
	// Init legs publisher and subscriber
	srcHost := test.MkTestHost()
	srcStore := dssync.MutexWrap(datastore.NewMapDatastore())
	fakeLsys := cidlink.DefaultLinkSystem()
	srcLnkS := test.MkLinkSystem(srcStore)

	gsnet := gsnet.NewFromLibp2pHost(srcHost)
	dtNet := dtnetwork.NewFromLibp2pHost(srcHost)
	gs := gsimpl.New(context.Background(), gsnet, fakeLsys)
	tp := gstransport.NewTransport(srcHost.ID(), gs, dtNet)

	// DataTransfer channels use this file to track cidlist of exchanges
	// NOTE: It needs to be initialized for the datatransfer not to fail, but
	// it has no other use outside the cidlist, so I don't think it should be
	// exposed publicly. It's only used for the life of a data transfer.
	// In the future, once an empty directory is accepted as input, it
	// this may be removed.
	tmpDir, err := ioutil.TempDir("", "go-legs")
	if err != nil {
		t.Fatal(err)
	}
	dt, err := datatransfer.NewDataTransfer(srcStore, tmpDir, dtNet, tp)
	if err != nil {
		t.Fatal(err)
	}
	err = dt.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lp, err := dtsync.NewPublisherFromExisting(context.Background(), dt, srcHost, testTopic, srcLnkS)
	if err != nil {
		t.Fatal(err)
	}

	dstHost := test.MkTestHost()
	srcHost.Peerstore().AddAddrs(dstHost.ID(), dstHost.Addrs(), time.Hour)
	dstHost.Peerstore().AddAddrs(srcHost.ID(), srcHost.Addrs(), time.Hour)
	dstStore := dssync.MutexWrap(datastore.NewMapDatastore())
	dstLnkS := test.MkLinkSystem(dstStore)
	ls, err := dtsync.NewSubscriber(context.Background(), dstHost, dstStore, dstLnkS, testTopic, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := srcHost.Connect(context.Background(), dstHost.Peerstore().PeerInfo(dstHost.ID())); err != nil {
		t.Fatal(err)
	}

	watcher, cncl := ls.OnChange()

	// Update root with item
	itm := basicnode.NewString("hello world")
	lnk, err := test.Store(srcStore, itm)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		cncl()
		lp.Close()
		ls.Close()
		os.RemoveAll(tmpDir)
	})

	// per https://github.com/libp2p/go-libp2p-pubsub/blob/e6ad80cf4782fca31f46e3a8ba8d1a450d562f49/gossipsub_test.go#L103
	// we don't seem to have a way to manually trigger needed gossip-sub heartbeats for mesh establishment.
	time.Sleep(2 * time.Second)

	if err := lp.UpdateRoot(context.Background(), lnk.(cidlink.Link).Cid); err != nil {
		t.Fatal(err)
	}

	select {
	case <-time.After(time.Second * 5):
		t.Fatal("timed out waiting for sync to propogate")
	case downstream := <-watcher:
		if !downstream.Equals(lnk.(cidlink.Link).Cid) {
			t.Fatalf("sync'd sid unexpected %s vs %s", downstream, lnk)
		}
		if _, err := dstStore.Get(datastore.NewKey(downstream.String())); err != nil {
			t.Fatalf("data not in receiver store: %v", err)
		}
	}
}

func TestSetAndFilterPeerPolicy(t *testing.T) {
	// Init legs publisher and subscriber
	srcStore := dssync.MutexWrap(datastore.NewMapDatastore())
	dstStore := dssync.MutexWrap(datastore.NewMapDatastore())
	_, dstHost, lp, ls := initPubSub(t, srcStore, dstStore)

	// Set policy to filter dstHost, which is not the one generating the update.
	err := ls.SetPolicyHandler(legs.FilterPeerPolicy(dstHost.ID()))
	if err != nil {
		t.Fatal(err)
	}
	// per https://github.com/libp2p/go-libp2p-pubsub/blob/e6ad80cf4782fca31f46e3a8ba8d1a450d562f49/gossipsub_test.go#L103
	// we don't seem to have a way to manually trigger needed gossip-sub heartbeats for mesh establishment.
	time.Sleep(2 * time.Second)

	watcher, cncl := ls.OnChange()

	// Update root with item
	np := basicnode.Prototype__Any{}
	nb := np.NewBuilder()
	ma, _ := nb.BeginMap(2)
	if err = ma.AssembleKey().AssignString("hey"); err != nil {
		t.Fatal(err)
	}
	if err = ma.AssembleValue().AssignString("it works!"); err != nil {
		t.Fatal(err)
	}
	if err = ma.AssembleKey().AssignString("yes"); err != nil {
		t.Fatal(err)
	}
	if err = ma.AssembleValue().AssignBool(true); err != nil {
		t.Fatal(err)
	}
	if err = ma.Finish(); err != nil {
		t.Fatal(err)
	}
	n := nb.Build()
	lnk, err := test.Store(srcStore, n)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		cncl()
		lp.Close()
		ls.Close()
	})

	if err = lp.UpdateRoot(context.Background(), lnk.(cidlink.Link).Cid); err != nil {
		t.Fatal(err)
	}

	select {
	case <-time.After(time.Second * 3):
	case <-watcher:
		t.Fatal("something was exchanged, and that is wrong")
	}
}
