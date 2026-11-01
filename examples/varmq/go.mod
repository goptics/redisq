module github.com/goptics/redisq/examples/varmq

go 1.26.0

require (
	github.com/goptics/redisq v0.0.0
	github.com/goptics/varmq v1.3.1
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/redis/go-redis/v9 v9.23.0 // indirect
	go.uber.org/atomic v1.12.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
)

replace github.com/goptics/redisq => ../../
