package firecracker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (m *Manager) allocateJailerUID(instanceID string) (int, error) {
	if m == nil || m.cfg == nil {
		return 0, fmt.Errorf("allocate jailer UID: manager configuration is required")
	}
	if m.cfg.MicroVMAllowUnjailed {
		return os.Getuid(), nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jailerUIDs == nil {
		m.jailerUIDs = map[int]string{}
	}
	for offset := 0; offset < m.cfg.MicroVMJailerUIDCount; offset++ {
		uid := m.cfg.MicroVMJailerUIDStart + offset
		if _, inUse := m.jailerUIDs[uid]; inUse {
			continue
		}
		m.jailerUIDs[uid] = instanceID
		return uid, nil
	}
	return 0, fmt.Errorf("allocate jailer UID: range %d..%d is exhausted", m.cfg.MicroVMJailerUIDStart, m.cfg.MicroVMJailerUIDStart+m.cfg.MicroVMJailerUIDCount-1)
}

func (m *Manager) reserveJailerUID(instanceID string, uid int) error {
	if m == nil || m.cfg == nil {
		return fmt.Errorf("reserve jailer UID: manager configuration is required")
	}
	if m.cfg.MicroVMAllowUnjailed {
		return nil
	}
	if uid < m.cfg.MicroVMJailerUIDStart || uid >= m.cfg.MicroVMJailerUIDStart+m.cfg.MicroVMJailerUIDCount {
		return fmt.Errorf("UID %d is outside configured range %d..%d", uid, m.cfg.MicroVMJailerUIDStart, m.cfg.MicroVMJailerUIDStart+m.cfg.MicroVMJailerUIDCount-1)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jailerUIDs == nil {
		m.jailerUIDs = map[int]string{}
	}
	if owner, exists := m.jailerUIDs[uid]; exists && owner != instanceID {
		return fmt.Errorf("UID %d is already reserved by instance %q", uid, owner)
	}
	m.jailerUIDs[uid] = instanceID
	return nil
}

func (m *Manager) releaseJailerUID(instanceID string, uid int) error {
	if m == nil || m.cfg == nil || m.cfg.MicroVMAllowUnjailed || uid == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	owner, exists := m.jailerUIDs[uid]
	if !exists {
		return nil
	}
	if owner != instanceID {
		return fmt.Errorf("release jailer UID %d for instance %q: reserved by %q", uid, instanceID, owner)
	}
	delete(m.jailerUIDs, uid)
	return nil
}

func (m *Manager) writeJailerUIDLease(runDir string, uid int) error {
	if m == nil || m.cfg == nil || m.cfg.MicroVMAllowUnjailed {
		return nil
	}
	path := filepath.Join(runDir, jailerUIDLeaseName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(strconv.Itoa(uid) + "\n"); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	return file.Close()
}

func (m *Manager) readJailerUIDLease(runDir string) (int, error) {
	data, err := os.ReadFile(filepath.Join(runDir, jailerUIDLeaseName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("%s is missing", jailerUIDLeaseName)
		}
		return 0, err
	}
	uid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", jailerUIDLeaseName, err)
	}
	if m == nil || m.cfg == nil || uid < m.cfg.MicroVMJailerUIDStart || uid >= m.cfg.MicroVMJailerUIDStart+m.cfg.MicroVMJailerUIDCount {
		return 0, fmt.Errorf("UID %d is outside configured jailer UID range", uid)
	}
	return uid, nil
}
