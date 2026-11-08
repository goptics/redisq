module github.com/goptics/redisq/examples/workflow

go 1.27

replace github.com/goptics/redisq => ../../

require (
	github.com/goptics/redisq v0.0.0
	github.com/goptics/varmq v1.10.1-0.20261108161945-6bc553db29c6
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/goptics/varmq/contract v0.6.0 // indirect
	github.com/redis/go-redis/v9 v9.23.0 // indirect
	go.uber.org/atomic v1.12.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
)
