package token

import (
	"context"
	"sync"
	"sync/atomic"
)

type keyedLocker struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	permit     chan struct{}
	references int
}

type keyedLease struct {
	locker   *keyedLocker
	key      string
	lock     *keyedLock
	released atomic.Bool
}

func newKeyedLocker() *keyedLocker {
	return &keyedLocker{locks: make(map[string]*keyedLock)}
}

func (l *keyedLocker) Acquire(ctx context.Context, key string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	l.mu.Lock()
	lock := l.locks[key]
	if lock == nil {
		lock = &keyedLock{permit: make(chan struct{}, 1)}
		lock.permit <- struct{}{}
		l.locks[key] = lock
	}
	lock.references++
	l.mu.Unlock()

	select {
	case <-ctx.Done():
		l.releaseReference(key, lock)
		return nil, ctx.Err()
	case <-lock.permit:
		if err := ctx.Err(); err != nil {
			l.release(key, lock)
			return nil, err
		}
		lease := &keyedLease{locker: l, key: key, lock: lock}
		return lease.Release, nil
	}
}

func (l *keyedLocker) release(key string, lock *keyedLock) {
	l.mu.Lock()
	select {
	case lock.permit <- struct{}{}:
	default:
		l.mu.Unlock()
		panic("token: keyed lock released without being acquired")
	}
	lock.references--
	if lock.references == 0 && l.locks[key] == lock {
		delete(l.locks, key)
	}
	l.mu.Unlock()
}

func (l *keyedLocker) releaseReference(key string, lock *keyedLock) {
	l.mu.Lock()
	lock.references--
	if lock.references == 0 && l.locks[key] == lock {
		delete(l.locks, key)
	}
	l.mu.Unlock()
}

func (lease *keyedLease) Release() {
	if !lease.released.CompareAndSwap(false, true) {
		panic("token: keyed lock released more than once")
	}
	lease.locker.release(lease.key, lease.lock)
}

func (l *keyedLocker) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.locks)
}
