package workspacestore

import "os"

func observeExclusiveBytes(_ *os.File) (*int64, string) {
	return nil, "fiemap_unsupported"
}
