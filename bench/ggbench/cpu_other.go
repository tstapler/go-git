//go:build !unix

package main

import "time"

// cpuTime is not available off Unix; the cpu column reads 0.
func cpuTime() time.Duration { return 0 }
