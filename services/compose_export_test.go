package services

import (
	"fmt"
	"net"
	"strings"
	"testing"
)

func TestPortableComposeUsesRequestedPortsEvenWhenGeneratorHostIsBusy(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	payload := BuildComposePayload{Services: []ServiceEntry{{Key: "nginx", Ports: []string{fmt.Sprintf("%d:80", port)}}}}
	for range 2 {
		compose := BuildComposeForExport(payload, "same-name")
		service := compose["services"].(M)["nginx"].(M)
		if got := fmt.Sprint(service["ports"]); !strings.Contains(got, fmt.Sprintf("%d:80", port)) {
			t.Fatalf("generator host changed download ports: %s", got)
		}
	}
}
