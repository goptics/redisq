package main

import (
	"fmt"
	"math/rand"
	"strconv"
	"time"

	"github.com/goptics/redisq"
	"github.com/goptics/varmq"
)

func generateJobID() string {
	const letters = "abcdefghijklmnodqrstuvwxyzABCDEFGHIJKLMNOdqRSTUVWXYZ0123456789"
	b := make([]byte, 8)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

func main() {
	start := time.Now()
	defer func() {
		fmt.Println("Time taken:", time.Since(start))
	}()

	redisQueue := redisq.New("redis://localhost:6375")
	defer redisQueue.Close()
	rq := redisQueue.NewDistributedPriorityQueue("scraping_priority_queue")
	dq := varmq.NewDistributedPriorityQueue[[]string](rq)
	defer dq.Close()

	for i := range 1000 {
		id := generateJobID()
		data := []string{fmt.Sprintf("https://example.com/%s", strconv.Itoa(i)), id}
		dq.Add(data, i%2, varmq.WithJobId(id))
	}

	fmt.Println("added jobs")
	fmt.Println("pending jobs:", dq.NumPending())

	// time.Sleep(5 * time.Second)
	// dq.Purge()
	// fmt.Println("purged jobs")
}
