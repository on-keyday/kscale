package workload

import (
	"context"
	"fmt"
	"time"

	criapi "github.com/on-keyday/kscale/protobuf/proto/cri"
)

// Image pulls run in the background, off the converge path. A pull is one unary
// CRI RPC that may legitimately take many minutes (multi-GB images); done inline
// it held the engine mutex (stalling Converge/List/Endpoints), kept the control
// plane's ApplyContainers waiting, and — the old container being torn down first —
// left the service down for the whole download. Now ensure() only recreates once
// the image is present; until then the current container keeps running and the
// pull proceeds here, kicking a converge when it lands.
//
// Pull policy is effectively IfNotPresent: an image already in the runtime is
// never re-pulled, so a mutable tag (":latest") does not pick up a newer push.
// Pin tags.

// Puller is the image-pull half of the CRI, separate from Runtime so pulls can run
// on their own connection (WithPuller): a CRI connection's receive timeout bounds
// — and on expiry aborts — every in-flight call on it, so a long pull sharing the
// converge connection would need a timeout that also lets a hung runtime stall
// everything else.
type Puller interface {
	PullImage(context.Context, *criapi.PullImageRequest) (*criapi.PullImageResponse, error)
}

// Option configures an Engine.
type Option func(*Engine)

// WithPuller routes image pulls through p instead of the Runtime.
func WithPuller(p Puller) Option { return func(e *Engine) { e.puller = p } }

// WithPullConcurrency caps how many images pull at once (default 2), so one huge
// image cannot starve the rest while several large ones don't saturate the link
// and disk together.
func WithPullConcurrency(n int) Option {
	return func(e *Engine) {
		if n > 0 {
			e.pullSem = make(chan struct{}, n)
		}
	}
}

// Retry backoff after a failed pull: pullBackoffBase doubling per consecutive
// failure, capped at pullBackoffMax.
const (
	pullBackoffBase = 10 * time.Second
	pullBackoffMax  = 5 * time.Minute
)

// pullState tracks one image's background pull. Absent from Engine.pulls = no
// pull wanted or the last one succeeded.
type pullState struct {
	inFlight bool
	err      error // last failure, kept until the next attempt succeeds
	failures int
	retryAt  time.Time
}

// imageReady reports whether image is present in the runtime. If it is not, a
// background pull is requested (deduplicated per image, delayed by backoff after
// a failure) and false is returned — the caller leaves the container as it is.
func (e *Engine) imageReady(ctx context.Context, image string) (bool, error) {
	st, err := e.rt.ImageStatus(ctx, &criapi.ImageStatusRequest{Image: &criapi.ImageSpec{Image: image}})
	if err != nil {
		return false, fmt.Errorf("image status %q: %w", image, err)
	}
	if st.Image != nil {
		return true, nil
	}
	e.requestPull(image)
	return false, nil
}

func (e *Engine) requestPull(image string) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	ps := e.pulls[image]
	if ps == nil {
		ps = &pullState{}
		e.pulls[image] = ps
	}
	if ps.inFlight || e.now().Before(ps.retryAt) {
		return
	}
	ps.inFlight = true
	e.pullWG.Add(1)
	go e.pull(image)
}

func (e *Engine) pull(image string) {
	defer e.pullWG.Done()
	select {
	case e.pullSem <- struct{}{}:
		defer func() { <-e.pullSem }()
	case <-e.pullCtx.Done():
		e.finishPull(image, e.pullCtx.Err())
		return
	}
	e.logger.Info("workload: pulling image", "image", image)
	start := e.now()
	_, err := e.puller.PullImage(e.pullCtx, &criapi.PullImageRequest{Image: &criapi.ImageSpec{Image: image}})
	if err != nil {
		e.logger.Warn("workload: image pull failed", "image", image, "error", err)
	} else {
		e.logger.Info("workload: image pulled", "image", image, "took", e.now().Sub(start).Round(time.Second))
	}
	e.finishPull(image, err)
}

func (e *Engine) finishPull(image string, err error) {
	e.pmu.Lock()
	ps := e.pulls[image]
	if err == nil {
		delete(e.pulls, image)
	} else if ps != nil {
		ps.inFlight = false
		ps.err = err
		ps.failures++
		// The shift is capped so it can't overflow; 10s<<5 already exceeds the max.
		ps.retryAt = e.now().Add(min(pullBackoffBase<<min(ps.failures-1, 5), pullBackoffMax))
	}
	e.pmu.Unlock()
	if err == nil {
		// Wake the owner's converge loop so the container starts now, not on the
		// next tick. Non-blocking: one pending kick covers any number of pulls.
		select {
		case e.pullDone <- struct{}{}:
		default:
		}
	}
}

// prunePulls forgets settled (not in-flight) pull records of images no desired
// spec uses any more, so a failure for a since-replaced image neither lingers
// nor delays a later re-request through its backoff. Called with e.mu held.
func (e *Engine) prunePulls() {
	want := map[string]bool{}
	for _, s := range e.desired {
		want[s.Image] = true
	}
	e.pmu.Lock()
	defer e.pmu.Unlock()
	for image, ps := range e.pulls {
		if !ps.inFlight && !want[image] {
			delete(e.pulls, image)
		}
	}
}

// PullInfo is a pending background pull as List reports it.
type PullInfo struct {
	State   string    // "" (none) | PullStatePulling | PullStateFailed
	Image   string    // the image being pulled
	Error   string    // PullStateFailed: the last attempt's error
	RetryAt time.Time // PullStateFailed: when the next attempt may start
}

// PullInfo.State values (the wire's pull_state).
const (
	PullStatePulling = "PULLING"
	PullStateFailed  = "FAILED"
)

// pullInfo reports image's background pull; zero if none is pending.
func (e *Engine) pullInfo(image string) PullInfo {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	ps := e.pulls[image]
	switch {
	case ps == nil:
		return PullInfo{}
	case ps.inFlight:
		return PullInfo{State: PullStatePulling, Image: image}
	case ps.err != nil:
		return PullInfo{State: PullStateFailed, Image: image, Error: ps.err.Error(), RetryAt: ps.retryAt}
	default:
		return PullInfo{}
	}
}

// PullDone signals after a background pull succeeds; the owner should Converge so
// the waiting container starts without waiting for its next periodic tick.
func (e *Engine) PullDone() <-chan struct{} { return e.pullDone }

// Close cancels in-flight pulls and waits for their goroutines to exit.
func (e *Engine) Close() {
	e.pullCancel()
	e.pullWG.Wait()
}
