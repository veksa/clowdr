package analytics

import (
	"io"
	"net/http"
	"strconv"
)

func ioReadFull(r *http.Request, b []byte) (int, error) { return io.ReadFull(r.Body, b) }

func itoa(n int) string { return strconv.Itoa(n) }
