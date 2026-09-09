//go:build !windows

package httpapi

import (
	"context"
	"errors"
)

func nativeVaultPickerAvailable() bool {
	return false
}

func pickNativeVaultDirectory(context.Context, string) (string, bool, error) {
	return "", false, errors.New("selector nativo de carpetas no disponible")
}
