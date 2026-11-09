// Workflow across processes: a DAG of jobs over redisq queues, run by
// varmq workers in other processes.
//
//	go run . worker upload,thumb      # process 1
//	go run . worker email,unsend      # process 2
//	go run . submit                   # submits, waits, prints the record
//
// REDIS_URL (default redis://localhost:6379) and REDIS_NS (namespace,
// default none) pick the Redis; WF_ID the workflow ID (default upload-1).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/goptics/redisq"
	"github.com/goptics/varmq"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: workflow worker <queue,...> | submit")
		os.Exit(2)
	}

	url := cmpOr(os.Getenv("REDIS_URL"), "redis://localhost:6379")
	var opts []redisq.Option
	if ns := os.Getenv("REDIS_NS"); ns != "" {
		opts = append(opts, redisq.WithNamespace(ns))
	}
	qs := redisq.New(url, opts...)
	defer qs.Close()

	// Every process needs the store: varmq finds it by scope to run and
	// finish tasks.
	store, err := redisq.NewWorkflowStore(qs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "store:", err)
		os.Exit(1)
	}
	defer store.Close()

	upload, thumb := qs.NewQueue("upload"), qs.NewPriorityQueue("thumb")
	email, unsend := qs.NewQueue("email"), qs.NewQueue("unsend")
	defer upload.Close()
	defer thumb.Close()
	defer email.Close()
	defer unsend.Close()

	switch os.Args[1] {
	case "worker":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: workflow worker <queue,...>")
			os.Exit(2)
		}
		names := strings.Split(os.Args[2], ",")
		var stops []func() error
		if slices.Contains(names, "upload") {
			w := varmq.NewResultWorker(func(j varmq.Job[string]) (string, error) {
				fmt.Println("upload", j.Data())
				return "s3://bucket/" + j.Data(), nil
			})
			w.WithQueue(upload)
			stops = append(stops, w.Stop)
		}
		if slices.Contains(names, "thumb") {
			w := varmq.NewResultWorker(func(j varmq.Job[int]) (string, error) {
				var src string
				if err := varmq.TaskOf(j).Result("upload", &src); err != nil {
					return "", err
				}
				fmt.Println("thumb", j.Data())
				return fmt.Sprintf("%s@%dpx", src, j.Data()), nil
			}, 2)
			w.WithPriorityQueue(thumb)
			stops = append(stops, w.Stop)
		}
		if slices.Contains(names, "email") {
			w := varmq.NewErrWorker(func(j varmq.Job[string]) error {
				t := varmq.TaskOf(j)
				for _, dep := range []string{"thumb-s", "thumb-l"} {
					var url string
					if err := t.Result(dep, &url); err != nil {
						return fmt.Errorf("no %s: %w", dep, err)
					}
					fmt.Println("email to", j.Data(), "with", url)
				}
				return nil
			})
			w.WithQueue(email)
			stops = append(stops, w.Stop)
		}
		if slices.Contains(names, "unsend") {
			w := varmq.NewErrWorker(func(j varmq.Job[string]) error {
				fmt.Println("undo email to", j.Data())
				return nil
			})
			w.WithQueue(unsend)
			stops = append(stops, w.Stop)
		}
		fmt.Println("ready", names)
		upload.Listen() // until SIGINT or SIGTERM
		for _, stop := range stops {
			_ = stop()
		}

	case "submit":
		ctx := context.Background()
		wf := varmq.NewWorkflow(store, varmq.WithWorkflowID(cmpOr(os.Getenv("WF_ID"), "upload-1")))
		up := wf.Add("upload", varmq.NewResultQueue[string, string](upload), "photo.png")
		small := wf.Add("thumb-s", varmq.NewResultPriorityQueue[int, string](thumb), 64).After(up).Priority(1)
		large := wf.Add("thumb-l", varmq.NewResultPriorityQueue[int, string](thumb), 512).After(up).Priority(2)
		wf.Add("email", varmq.NewErrQueue[string](email), "ann@example.com").
			After(small, large).
			Undo(varmq.NewErrQueue[string](unsend), "ann@example.com") // runs if the workflow fails or is cancelled after email
		if err := wf.Submit(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "submit:", err)
			os.Exit(1)
		}

		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		rec, err := wf.WaitContext(wctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "wait:", err)
			os.Exit(1)
		}
		out, _ := json.MarshalIndent(rec, "", "  ")
		fmt.Println(string(out))
		if rec.State != varmq.WorkflowCompleted {
			os.Exit(1)
		}

	default:
		fmt.Fprintln(os.Stderr, "unknown command", os.Args[1])
		os.Exit(2)
	}
}

func cmpOr(v, def string) string {
	if v != "" {
		return v
	}
	return def
}
