package main

import (
	"fmt"
	"time"

	"github.com/goptics/redisq/v2"
	"github.com/goptics/varmq"
)

func main() {
	start := time.Now()
	defer func() {
		fmt.Println("Time taken:", time.Since(start))
	}()

	redisQueue := redisq.New("redis://localhost:6375")
	rq := redisQueue.NewDistributedPriorityQueue("scraping_priority_queue")

	w := varmq.NewWorker(func(j varmq.Job[[]string]) {
		data := j.Data()
		url, id := data[0], data[1]
		fmt.Printf("Scraping url: %s, id: %s\n", url, id)
		time.Sleep(2 * time.Second)
		fmt.Printf("Scraped url: %s, id: %s\n", url, id)
	}, 5)

	// Using redisq adapter (you can use any adapter that implements IDistributedQueue)
	q := w.WithDistributedPriorityQueue(rq)

	fmt.Println("pending jobs:", q.NumPending())
	fmt.Println("listening...")

	rq.Listen() // until SIGINT or SIGTERM
	w.Stop()    // finish the jobs in progress
	rq.Close()  // and hand any unacknowledged one to another consumer at once
}
