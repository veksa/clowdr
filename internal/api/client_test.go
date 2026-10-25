package api

import (
	"context"
	"strings"
	"testing"
)

func TestClientRefusesPlainHTTP(t *testing.T) {
	for _, s := range []string{"http://deploy.example.com", "http://203.0.113.10:7070"} {
		err := NewClient(s, "secret", "").Do(context.Background(), "GET", "/v1/version", nil, nil)
		if err == nil || !strings.Contains(err.Error(), "use https") {
			t.Errorf("%s: %v", s, err)
		}
	}
}
