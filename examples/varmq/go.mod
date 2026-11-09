module github.com/goptics/redisq/examples/varmq

go 1.27

require (
	github.com/goptics/redisq v0.0.0
	github.com/goptics/varmq v1.10.1-0.20261109083602-65f2a1fb0f32
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/goptics/varmq/contract v0.7.0 // indirect
	github.com/redis/go-redis/v9 v9.23.0 // indirect
	go.uber.org/atomic v1.12.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
)

replace github.com/goptics/redisq => ../../
