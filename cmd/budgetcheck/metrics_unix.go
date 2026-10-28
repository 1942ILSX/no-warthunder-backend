//go:build linux || darwin

package main

import (
	"runtime"
	"syscall"
)

type usage struct {
	CPUSeconds   float64
	PeakRSSBytes int64
}

func processUsage() (usage, error) {
	var r syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &r); err != nil {
		return usage{}, err
	}
	rss := int64(r.Maxrss)
	if runtime.GOOS == "linux" {
		rss *= 1024
	}
	cpu := float64(r.Utime.Sec+r.Stime.Sec) + float64(r.Utime.Usec+r.Stime.Usec)/1e6
	return usage{CPUSeconds: cpu, PeakRSSBytes: rss}, nil
}
