package redisq

import (
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestNotification(t *testing.T) (*Notification, func()) {
	redisURL := getTestRedisURL()
	opts, err := redis.ParseURL(redisURL)
	require.NoError(t, err)

	client := redis.NewClient(opts)
	n := newNotification(client, testQueueKey)

	cleanup := func() {
		n.Stop()
		client.Close()
	}

	return n, cleanup
}

func TestNotificationSubscribe(t *testing.T) {
	n, cleanup := setupTestNotification(t)
	defer cleanup()

	received := make(chan struct{})
	expectedAction := "test_action"

	n.Subscribe(func(action string) {
		assert.Equal(t, expectedAction, action)
		received <- struct{}{}
	})

	n.Start()

	// Send notification
	n.Send(expectedAction)

	select {
	case <-received:
		// Test passed
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for notification")
	}
}

func TestNotificationMultipleSubscribers(t *testing.T) {
	n, cleanup := setupTestNotification(t)
	defer cleanup()

	numSubscribers := 3
	received := make(chan struct{}, numSubscribers)

	for i := 0; i < numSubscribers; i++ {
		n.Subscribe(func(action string) {
			received <- struct{}{}
		})
	}

	n.Start()

	// Send notification
	n.Send("test")

	// Wait for all subscribers
	for i := 0; i < numSubscribers; i++ {
		select {
		case <-received:
			// Subscriber received notification
		case <-time.After(time.Second):
			t.Fatal("Timeout waiting for notification")
		}
	}
}

func TestNotificationStop(t *testing.T) {
	n, cleanup := setupTestNotification(t)
	defer cleanup()

	received := make(chan struct{})
	n.Subscribe(func(action string) {
		received <- struct{}{}
	})

	n.Start()
	n.Stop()

	// Send notification after stop
	n.Send("test")

	select {
	case <-received:
		t.Fatal("Should not receive notification after stop")
	case <-time.After(100 * time.Millisecond):
		// Test passed
	}
}
