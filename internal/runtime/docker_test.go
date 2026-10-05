package runtime

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func frame(stream byte, s string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

func TestDemux(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(1, "hel"))
	buf.Write(frame(2, "err1\n"))
	buf.Write(frame(1, "lo\nworld\npart"))
	var got []string
	err := Demux(&buf, func(stderr bool, line string) {
		if stderr {
			line = "E:" + line
		}
		got = append(got, line)
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != "E:err1|hello|world|part" {
		t.Fatal(got)
	}
}

func TestClientErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/nope/json"):
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]string{"message": "No such container: nope"})
		case strings.HasSuffix(r.URL.Path, "/info"):
			json.NewEncoder(w).Encode(map[string]any{
				"ServerVersion": "29.0.1",
				"DriverStatus":  [][2]string{{"driver-type", "io.containerd.snapshotter.v1"}},
			})
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(srv.URL, "http://"))
	d, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := d.ContainerInspect(ctx, "nope"); !errorsIsNotFound(err) {
		t.Fatalf("want not found, got %v", err)
	}
	if err := d.ContainerRemove(ctx, "nope2"); err == nil {
		t.Fatal("500 must be an error")
	}
	info, err := d.Info(ctx)
	if err != nil || !info.ContainerdStore() || info.ServerVersion != "29.0.1" {
		t.Fatal(info, err)
	}
}

func errorsIsNotFound(err error) bool {
	ae, ok := err.(*APIError)
	return ok && ae.Is(ErrNotFound) && strings.Contains(ae.Message, "No such container")
}
