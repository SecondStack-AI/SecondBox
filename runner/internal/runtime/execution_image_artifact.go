package runtimemanager

import (
	"fmt"
	"os"
	"reflect"
)

// VerifiedExecutionImageArtifact binds a verified image file to its filesystem identity.
type VerifiedExecutionImageArtifact struct {
	Label           string
	Path            string
	Device          uint64
	Inode           uint64
	Size            int64
	ModTimeUnixNano int64
	ChangeUnixNano  int64
}

func CaptureVerifiedExecutionImageArtifact(label, path string) (VerifiedExecutionImageArtifact, error) {
	info, err := os.Stat(path)
	if err != nil {
		return VerifiedExecutionImageArtifact{}, err
	}
	device, inode, changed, ok := executionImageFileIdentity(info)
	if !ok {
		return VerifiedExecutionImageArtifact{}, fmt.Errorf("unsupported stat metadata")
	}
	return VerifiedExecutionImageArtifact{
		Label: label, Path: path, Device: device, Inode: inode, Size: info.Size(),
		ModTimeUnixNano: info.ModTime().UnixNano(), ChangeUnixNano: changed,
	}, nil
}

func executionImageFileIdentity(info os.FileInfo) (device uint64, inode uint64, changed int64, ok bool) {
	if info == nil || info.Sys() == nil {
		return 0, 0, 0, false
	}
	stat := reflect.Indirect(reflect.ValueOf(info.Sys()))
	if !stat.IsValid() {
		return 0, 0, 0, false
	}
	device, ok = executionImageUint64Field(stat.FieldByName("Dev"))
	if !ok {
		return 0, 0, 0, false
	}
	inode, ok = executionImageUint64Field(stat.FieldByName("Ino"))
	if !ok {
		return 0, 0, 0, false
	}
	changedField := stat.FieldByName("Ctim")
	if !changedField.IsValid() {
		changedField = stat.FieldByName("Ctimespec")
	}
	if !changedField.IsValid() {
		return 0, 0, 0, false
	}
	seconds, secondsOK := executionImageInt64Field(changedField.FieldByName("Sec"))
	nanoseconds, nanosecondsOK := executionImageInt64Field(changedField.FieldByName("Nsec"))
	if !secondsOK || !nanosecondsOK {
		return 0, 0, 0, false
	}
	return device, inode, seconds*1_000_000_000 + nanoseconds, true
}

func executionImageUint64Field(field reflect.Value) (uint64, bool) {
	if !field.IsValid() {
		return 0, false
	}
	switch field.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value := field.Int()
		if value < 0 {
			return 0, false
		}
		return uint64(value), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return field.Uint(), true
	default:
		return 0, false
	}
}

func executionImageInt64Field(field reflect.Value) (int64, bool) {
	if !field.IsValid() {
		return 0, false
	}
	switch field.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return field.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		value := field.Uint()
		if value > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(value), true
	default:
		return 0, false
	}
}
