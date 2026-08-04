//go:build !linux && !darwin

package agentapp

import "os"

func preserveFileOwner(_ string, _ os.FileInfo) error { return nil }
