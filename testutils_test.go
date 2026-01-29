package redisq

import (
	"fmt"
	"log"
	"os"
	"testing"

	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
)

var testRedisURL string

func TestMain(m *testing.M) {
	// Uses a sensible default on windows (tcp/http) and linux/osx (socket)
	pool, err := dockertest.NewPool("")
	if err != nil {
		log.Fatalf("Could not construct pool: %s", err)
	}

	// Uses pool to try to connect to Docker
	err = pool.Client.Ping()
	if err != nil {
		log.Fatalf("Could not connect to Docker: %s", err)
	}

	// Pulls an image, creates a container based on it and runs it
	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "redis",
		Tag:        "latest",
	}, func(config *docker.HostConfig) {
		// Set AutoRemove to true so that stopped container goes away by itself
		config.AutoRemove = true
		config.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		log.Fatalf("Could not start resource: %s", err)
	}

	// Tell docker to hard kill the container in 120 seconds
	resource.Expire(120)

	// Get the mapped port
	port := resource.GetPort("6379/tcp")
	testRedisURL = fmt.Sprintf("redis://localhost:%s", port)

	// Exponential backoff-retry, because the application in the container
	// might not be ready to accept connections yet
	if err := pool.Retry(func() error {
		qs := New(testRedisURL)
		defer qs.Close()
		return nil
	}); err != nil {
		log.Fatalf("Could not connect to Redis: %s", err)
	}

	// Run tests
	code := m.Run()

	// Cleanup - You can't defer this because os.Exit doesn't care for defer
	if err := pool.Purge(resource); err != nil {
		log.Fatalf("Could not purge resource: %s", err)
	}

	os.Exit(code)
}

// getTestRedisURL returns the Redis URL for the dockertest-managed container
func getTestRedisURL() string {
	return testRedisURL
}
