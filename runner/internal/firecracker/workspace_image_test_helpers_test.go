package firecracker

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

func debugfsReadFile(ctx context.Context, imagePath, guestPath string) ([]byte, bool, error) {
	out, err := exec.CommandContext(ctx, "debugfs", "-R", "cat "+guestPath, imagePath).CombinedOutput()
	text := string(out)
	if strings.Contains(text, "File not found") || strings.Contains(text, "File not found by ext2_lookup") {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("debugfs cat %s: %w: %s", guestPath, err, strings.TrimSpace(text))
	}
	lines := strings.SplitN(text, "\n", 2)
	if len(lines) == 2 && strings.HasPrefix(lines[0], "debugfs ") {
		text = lines[1]
	}
	return []byte(text), true, nil
}
