package database

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
)

var Rdb *redis.Client

func InitRedis() error {
	redisURL := os.Getenv("REDIS_URL")
	redisAddr := os.Getenv("REDIS_ADDR")
	password := os.Getenv("REDIS_PASSWORD")

	if redisURL != "" {
		opts, err := redis.ParseURL(redisURL)
		if err != nil {
			return fmt.Errorf("invalid REDIS_URL: %w", err)
		}
		// Allow overriding password via REDIS_PASSWORD when URL omits it
		if opts.Password == "" && password != "" {
			opts.Password = password
		}
		Rdb = redis.NewClient(opts)
	} else {
		if redisAddr == "" {
			return fmt.Errorf("redis is not configured: set REDIS_URL or REDIS_ADDR")
		}
		Rdb = redis.NewClient(&redis.Options{
			Addr:     redisAddr,
			Password: password,
			DB:       0,
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := Rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("failed to connect to Redis: %w", err)
	}

	fmt.Println("Connected to Redis successfully")
	return nil
}
