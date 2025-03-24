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
	subscribers []func(action string, message []byte)
	mx          sync.RWMutex
	ctx         context.Context
	cancel      context.CancelFunc
}

func NewNotification(client *redis.Client, key string) *Notification {
	ctx, cancel := context.WithCancel(context.Background())
	notificationKey := key + ":notifications"

	return &Notification{
		client:      client,
		pubsub:      client.Subscribe(ctx, notificationKey),
		cancel:      cancel,
		key:         notificationKey,
		subscribers: make([]func(action string, message []byte), 0),
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

	event, err := parseToEvent([]byte(msg))
	if err != nil {
		log.Printf("Error parsing event: %v", err)
		return
	}

	for _, subscriber := range n.subscribers {
		subscriber(event.Action, event.Message)
	}
}

func (n *Notification) Subscribe(handler func(action string, message []byte)) {
	n.mx.Lock()
	n.subscribers = append(n.subscribers, handler)
	n.mx.Unlock()
}

func (n *Notification) Send(action string, message []byte) {
	event := Event{Action: action, Message: message}
	bytes, _ := event.Json()

	if err := n.client.Publish(n.ctx, n.key, bytes).Err(); err != nil {
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
