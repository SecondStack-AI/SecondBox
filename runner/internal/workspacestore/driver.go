package workspacestore

import (
	"context"
	"os"
)

// platformDriver is the only platform-specific filesystem authority used by
// the common WorkspaceStore state machine. It never receives logical manifests
// or operation receipts.
type platformDriver interface {
	Clone(destination *os.File, source *os.File) error
	Format(context.Context, *os.File, int64, string) error
	SetUUID(context.Context, *os.File, string) error
	OpenAttachment(string) (*os.File, error)
	LinkDescriptor(*os.File, string) error
	ResetSparse(*os.File, int64) error
	TryLock(*os.File) error
	Unlock(*os.File) error
	SyncDirectory(string) error
	ChildDescriptorPath(int) string
}

type injectedDriver struct {
	cloner    imageCloner
	formatter imageFormatter
}

func (driver injectedDriver) TryLock(file *os.File) error     { return platformTryLock(file) }
func (driver injectedDriver) Unlock(file *os.File) error      { return platformUnlock(file) }
func (driver injectedDriver) SyncDirectory(path string) error { return platformSyncDirectory(path) }
func (driver injectedDriver) ChildDescriptorPath(descriptor int) string {
	return platformChildDescriptorPath(descriptor)
}

func (driver injectedDriver) Clone(destination *os.File, source *os.File) error {
	return driver.cloner.Clone(destination, source)
}

func (driver injectedDriver) Format(ctx context.Context, file *os.File, _ int64, uuid string) error {
	return driver.formatter.Format(ctx, file.Name(), uuid)
}

func (driver injectedDriver) SetUUID(ctx context.Context, file *os.File, uuid string) error {
	return driver.formatter.SetUUID(ctx, file.Name(), uuid)
}

func (driver injectedDriver) OpenAttachment(path string) (*os.File, error) {
	return platformOpenAttachment(path)
}

func (driver injectedDriver) LinkDescriptor(file *os.File, destination string) error {
	return platformLinkDescriptor(file, destination)
}

func (injectedDriver) ResetSparse(file *os.File, capacity int64) error {
	if err := file.Truncate(0); err != nil {
		return err
	}
	return file.Truncate(capacity)
}
