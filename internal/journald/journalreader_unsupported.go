//go:build !linux
// +build !linux

package journald

import (
	"errors"

	"github.com/metal-toolbox/audito-maldito/internal/util"
)

func newJournalReader(bootID string, distro util.DistroType) (JournalReader, error) {
	return nil, errors.New("unsupported platform")
}