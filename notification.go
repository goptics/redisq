package redisq

import (
	"context"
	"log"
	"sync"

	"github.com/redis/go-redis/v9"
)

type Notification struct {
	client      *redis.Client
	pubsub      *redis.PubSub
	key         string
	subscribers []func(action string)
	mx          sync.RWMutex
	ctx         context.Context
	cancel      context.CancelFunc
}

func newNotification(client *redis.Client, key string) *Notification {
	ctx, cancel := context.WithCancel(context.Background())
	notificationKey := key + ":notifications"

	return &Notification{
		client:      client,
		pubsub:      client.Subscribe(ctx, notificationKey),
		cancel:      cancel,
		key:         notificationKey,
		subscribers: make([]func(action string), 0),
		ctx:         ctx,
	}
}

func (n *Notification) Start() {
	go func() {
		for msg := range n.pubsub.Channel() {
			if msg == nil {
				continue
			}
			n.broadcast(msg.Payload)
		}
	}()
}

func (n *Notification) broadcast(msg string) {
	n.mx.RLock()
	defer n.mx.RUnlock()

	for _, subscriber := range n.subscribers {
		subscriber(msg)
	}
}

func (n *Notification) Subscribe(handler func(action string)) {
	n.mx.Lock()
	defer n.mx.Unlock()

	n.subscribers = append(n.subscribers, handler)
}

func (n *Notification) Send(action string) {
	if err := n.client.Publish(n.ctx, n.key, action).Err(); err != nil {
		log.Printf("Error publishing notification: %v", err)
	}
}

func (n *Notification) Stop() {
	n.cancel()
	n.pubsub.Close()

	n.mx.Lock()
	n.subscribers = nil
	n.mx.Unlock()
}
