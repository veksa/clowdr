package spec

import "testing"

func TestPaths(t *testing.T) {
	for p, ok := range map[string]bool{
		"/":              true,
		"/api/v2/":       true,
		"/a-b_c.d~e@f:g": true,
		"api":            false,
		"/a b":           false,
		"/a\tb":          false,
		"/a\rb":          false,
		"/a\b":           false,
		"/a#b":           false,
		"/a;b":           false,
		"/a{b}":          false,
		"/a$b":           false,
		"/a?b":           false,
	} {
		s := Service{Name: "web", Port: 80, Routes: []Route{{Path: p, Service: "api"}}}
		s.Defaults()
		if err := s.Validate(); (err == nil) != ok {
			t.Errorf("route %q: %v", p, err)
		}
	}
	for p, ok := range map[string]bool{"/health": true, "/health?ready=1": true, "/h\r\nX: y": false, "health": false} {
		s := Service{Name: "web", Port: 80, Health: Health{Path: p}}
		s.Defaults()
		if err := s.Validate(); (err == nil) != ok {
			t.Errorf("health %q: %v", p, err)
		}
	}
}
