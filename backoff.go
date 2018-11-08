package main

import (
	"errors"
	"log"
	"math"
	"runtime/debug"
	"time"
)

var (
	ErrBackoffRetry = errors.New("backoff: retry")
)

func backoff(f func() error, n ...int) chan struct{} {
	stack := debug.Stack()

	ret := make(chan struct{})
	go func() {
		defer close(ret)

		for i := 0; len(n) == 0 || i < n[0]; i++ {
			err := f()
			if err == nil {
				return
			}

			if err != ErrBackoffRetry {
				log.Printf("backoff %d: %s\n\n%s", i+1, err.Error(), stack)
			}

			<-time.After(time.Duration(math.Pow(2, float64(i))) * time.Second)
		}
	}()
	return ret
}
