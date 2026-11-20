package endpoint

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
)

func TestCredentialKeysDoNotCollideForDistinctHTTPDestinations(t *testing.T) {
	for _, pair := range [][2]string{
		{"https://api.openai.com/v1", "https://api.openaİ.com/v1"},
		{"https://[fe80::1%25Eth0]/v1", "https://[fe80::1%25eth0]/v1"},
	} {
		var addresses []string
		transport := &http.Transport{DialContext: func(_ context.Context, _, address string) (net.Conn, error) {
			addresses = append(addresses, address)
			return nil, errors.New("test dial stopped before network access")
		}}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport}
		for _, raw := range pair {
			if _, err := client.Get(raw); err == nil {
				t.Fatal("unexpected network success")
			}
		}
		if len(addresses) != 2 || addresses[0] == addresses[1] {
			t.Fatalf("fixture does not distinguish HTTP destinations: %v", addresses)
		}
		if Key(pair[0]) == Key(pair[1]) {
			t.Fatalf("distinct transport destinations share a credential key: %v", addresses)
		}
	}
}
