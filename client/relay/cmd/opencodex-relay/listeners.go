package main

import (
	"fmt"
	"net"
)

// listenPair binds both loopback listeners before either HTTP server starts.
// This prevents a half-started relay when the interactive port is unavailable.
func listenPair(generalAddress, interactiveAddress string) (net.Listener, net.Listener, error) {
	general, err := net.Listen("tcp", generalAddress)
	if err != nil {
		return nil, nil, fmt.Errorf("listen on general address: %w", err)
	}
	interactive, err := net.Listen("tcp", interactiveAddress)
	if err != nil {
		_ = general.Close()
		return nil, nil, fmt.Errorf("listen on interactive address: %w", err)
	}
	return general, interactive, nil
}
