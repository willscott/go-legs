package httpsync

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"sync"

	"github.com/filecoin-project/go-legs"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	"github.com/ipld/go-ipld-prime"
	"github.com/ipld/go-ipld-prime/codec/dagjson"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	basicnode "github.com/ipld/go-ipld-prime/node/basic"
	ma "github.com/multiformats/go-multiaddr"
)

type publisher struct {
	rl   sync.RWMutex
	root cid.Cid
	lsys ipld.LinkSystem
}

var _ legs.Publisher = (*publisher)(nil)
var _ http.Handler = (*publisher)(nil)

// NewPublisher creates a new http publisher
func NewPublisher(ctx context.Context, ds datastore.Batching, lsys ipld.LinkSystem) (legs.Publisher, error) {
	p := &publisher{}
	p.lsys = lsys
	return p, nil
}

func (p *publisher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ask := path.Base(r.URL.Path)
	if ask == "head" {
		// serve the
		p.rl.RLock()
		defer p.rl.RUnlock()
		out, err := json.Marshal(p.root.String())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			log.Errorw("Failed to serve root", "err", err)
		} else {
			_, _ = w.Write(out)
		}
		return
	}
	// interpret `ask` as a CID to serve.
	c, err := cid.Parse(ask)
	if err != nil {
		http.Error(w, "invalid request: not a cid", http.StatusBadRequest)
		return
	}
	item, err := p.lsys.Load(ipld.LinkContext{}, cidlink.Link{Cid: c}, basicnode.Prototype.Any)
	if err != nil {
		if errors.Is(err, ipld.ErrNotExists{}) {
			http.Error(w, "cid not found", http.StatusNotFound)
			return
		}
		http.Error(w, "unable to load data for cid", http.StatusInternalServerError)
		log.Errorw("Failed to load requested block", "err", err)
		return
	}
	// marshal to json and serve.
	_ = dagjson.Encode(item, w)
}

func (p *publisher) UpdateRoot(ctx context.Context, c cid.Cid) error {
	p.rl.Lock()
	defer p.rl.Unlock()
	p.root = c
	return nil
}

func (p *publisher) UpdateRootWithAddrs(ctx context.Context, c cid.Cid, _ []ma.Multiaddr) error {
	return p.UpdateRoot(ctx, c)
}

func (p *publisher) Close() error {
	return nil
}
