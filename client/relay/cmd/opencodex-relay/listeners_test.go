package main

import (
	"net"
	"testing"
)

func TestListenPairClosesGeneralWhenInteractiveBindFails(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network listeners are unavailable: %v", err)
	}
	defer occupied.Close()

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	generalAddress := probe.Addr().String()
	_ = probe.Close()

	general, interactive, err := listenPair(generalAddress, occupied.Addr().String())
	if err == nil || general != nil || interactive != nil {
		t.Fatalf("listenPair = (%v, %v, %v), want nil listeners and error", general, interactive, err)
	}

	rebound, err := net.Listen("tcp", generalAddress)
	if err != nil {
		t.Fatalf("general listener was left open after interactive failure: %v", err)
	}
	_ = rebound.Close()
}

func TestListenPairBindsBothAddresses(t *testing.T) {
	firstProbe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network listeners are unavailable: %v", err)
	}
	firstAddress := firstProbe.Addr().String()
	_ = firstProbe.Close()
	secondProbe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	secondAddress := secondProbe.Addr().String()
	_ = secondProbe.Close()

	general, interactive, err := listenPair(firstAddress, secondAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer general.Close()
	defer interactive.Close()
}
