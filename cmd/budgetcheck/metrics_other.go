//go:build !linux && !darwin

package main

import "errors"

type usage struct {
	CPUSeconds   float64
	PeakRSSBytes int64
}

func processUsage() (usage, error) {
	return usage{}, errors.New("budgetcheck resource sampling supports Linux and macOS")
}
