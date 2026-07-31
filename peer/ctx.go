package peer

import (
	"context"
	"errors"
)

type peerKey struct{}

func GetPeer(ctx context.Context) *Peer {
	if p, ok := ctx.Value(peerKey{}).(*Peer); ok {
		return p
	}
	return nil
}

func WithPeer(ctx context.Context, p *Peer) context.Context {
	return context.WithValue(ctx, peerKey{}, p)
}

var ErrNoPeerInContext = errors.New("no peer in context")
