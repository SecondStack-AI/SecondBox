package workspacestore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// tune2fsFreshCheckRequired is the text tune2fs prints, and exits 1 on, when
// a metadata_csum filesystem has been mounted since its last e2fsck. A Snapshot
// taken from a Workspace that a guest mounted is always in that state, so a
// cross-Sandbox clone must be checked before its UUID can be rewritten. A
// never-mounted template passes on the first attempt, keeping the create path
// free of any fsck cost.
const tune2fsFreshCheckRequired = "requires a freshly checked filesystem"

// rewriteExt4UUID sets the filesystem UUID of the ext4 image behind workspace,
// which the caller owns exclusively and has not mounted. descriptorPath is the
// platform's path to child descriptor 3.
func rewriteExt4UUID(
	ctx context.Context,
	setUUIDExecutable string,
	checkExecutable string,
	descriptorPath string,
	workspace *os.File,
	uuid string,
) error {
	output, err := runExt4Tool(ctx, setUUIDExecutable, workspace, "-U", uuid, descriptorPath)
	if err == nil {
		return nil
	}
	if !strings.Contains(output, tune2fsFreshCheckRequired) {
		return fmt.Errorf("SecondBox WorkspaceStore ext4 UUID rewrite failed: %w: %s", err, output)
	}
	// -f forces a full check of a clean-looking filesystem; -y answers every
	// repair prompt because no operator is attached. Exit status 1 means
	// errors were corrected, which is acceptable for an exclusively owned copy.
	checkOutput, checkErr := runExt4Tool(ctx, checkExecutable, workspace, "-f", "-y", descriptorPath)
	var exit *exec.ExitError
	if checkErr != nil && (!errors.As(checkErr, &exit) || exit.ExitCode() > 1) {
		return fmt.Errorf("SecondBox WorkspaceStore ext4 check before UUID rewrite failed: %w: %s", checkErr, checkOutput)
	}
	if output, err = runExt4Tool(ctx, setUUIDExecutable, workspace, "-U", uuid, descriptorPath); err != nil {
		return fmt.Errorf("SecondBox WorkspaceStore ext4 UUID rewrite failed after check: %w: %s", err, output)
	}
	return nil
}

func runExt4Tool(ctx context.Context, executable string, workspace *os.File, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, executable, arguments...)
	command.ExtraFiles = []*os.File{workspace}
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}
