//go:build !linux

package monitor

import (
	"context"
	"errors"
)

func followKernelOOM(context.Context, func(id string)) error {
	return errors.New("no kernel log on this system")
}
