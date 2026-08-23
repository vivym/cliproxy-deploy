package worker

import (
	"context"
	"errors"
	"sync"
	"time"
)

const requestPacerSchedulingMargin = time.Millisecond

type RequestPacer interface {
	Wait(context.Context) error
}

type intervalRequestPacer struct {
	minimumInterval time.Duration

	mutex         sync.Mutex
	nextRequestAt time.Time
}

func NewRequestPacer(minimumInterval time.Duration) (RequestPacer, error) {
	if minimumInterval < 0 {
		return nil, errors.New("request interval must not be negative")
	}
	return &intervalRequestPacer{minimumInterval: minimumInterval}, nil
}

func (p *intervalRequestPacer) Wait(ctx context.Context) error {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	wait := time.Until(p.nextRequestAt)
	if wait <= 0 {
		p.nextRequestAt = nextPacedRequest(time.Now(), p.minimumInterval)
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		p.nextRequestAt = nextPacedRequest(time.Now(), p.minimumInterval)
		return nil
	}
}

func nextPacedRequest(now time.Time, minimumInterval time.Duration) time.Time {
	if minimumInterval == 0 {
		return now
	}
	return now.Add(minimumInterval + requestPacerSchedulingMargin)
}
