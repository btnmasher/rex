package discord

import (
	"sync"
	"time"
)

const maxWebhookCooldownEntries = 1024

type webhookCooldowns struct {
	mu      sync.Mutex
	entries map[string]time.Time
}

func newWebhookCooldowns() *webhookCooldowns {
	return &webhookCooldowns{entries: make(map[string]time.Time)}
}

func (c *webhookCooldowns) remaining(key string) time.Duration {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	c.evictExpired(now)
	return max(c.entries[key].Sub(now), 0)
}

func (c *webhookCooldowns) set(key string, delay time.Duration) {
	if c == nil || delay <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	c.evictExpired(now)
	if _, exists := c.entries[key]; !exists && len(c.entries) >= maxWebhookCooldownEntries {
		return
	}
	allowedAt := now.Add(delay)
	if allowedAt.After(c.entries[key]) {
		c.entries[key] = allowedAt
	}
}

func (c *webhookCooldowns) evictExpired(now time.Time) {
	for key, allowedAt := range c.entries {
		if !now.Before(allowedAt) {
			delete(c.entries, key)
		}
	}
}
