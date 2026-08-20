//! Descriptor-only ext4 formatting for WorkspaceStore.

use std::{fs::File, io, os::fd::AsRawFd, process::Command};

use thiserror::Error;

#[derive(Debug, Error)]
pub enum FormatError {
    #[error("SecondBox Microsandbox Workspace format input is invalid")]
    Invalid,
    #[error("SecondBox Microsandbox Workspace format I/O: {0}")]
    Io(#[from] io::Error),
    #[error("SecondBox Microsandbox Workspace formatter failed: {0}")]
    Formatter(String),
}

pub fn format_workspace(
    image: &File,
    logical_capacity: u64,
    label: &str,
    uuid: [u8; 16],
) -> Result<(), FormatError> {
    if logical_capacity < 64 * 1024 * 1024
        || !logical_capacity.is_multiple_of(4096)
        || label.is_empty()
        || label.len() > 16
        || !label.is_ascii()
    {
        return Err(FormatError::Invalid);
    }
    image.set_len(logical_capacity)?;
    clear_cloexec(image)?;
    let descriptor = crate::fd::reopen_path(image.as_raw_fd());
    let uuid = format_uuid(uuid);
    let output = Command::new("mke2fs")
        .args([
            "-q",
            "-t",
            "ext4",
            "-F",
            "-U",
            &uuid,
            "-L",
            label,
            "-E",
            "lazy_itable_init=0,lazy_journal_init=0",
            &descriptor,
        ])
        .output()?;
    if !output.status.success() {
        return Err(FormatError::Formatter(
            String::from_utf8_lossy(&output.stderr)
                .trim()
                .chars()
                .take(4096)
                .collect(),
        ));
    }
    image.sync_all()?;
    let check = Command::new("e2fsck").args(["-fn", &descriptor]).output()?;
    if !check.status.success() && check.status.code() != Some(1) {
        return Err(FormatError::Formatter(
            "e2fsck rejected formatted Workspace".into(),
        ));
    }
    image.sync_all()?;
    Ok(())
}

fn clear_cloexec(file: &File) -> Result<(), io::Error> {
    let fd = file.as_raw_fd();
    // SAFETY: fcntl only reads and updates flags for the live borrowed descriptor.
    let flags = unsafe { libc::fcntl(fd, libc::F_GETFD) };
    if flags < 0 {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: the same live descriptor is retained for the duration of each child process.
    if unsafe { libc::fcntl(fd, libc::F_SETFD, flags & !libc::FD_CLOEXEC) } < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

fn format_uuid(uuid: [u8; 16]) -> String {
    let hex: String = uuid.iter().map(|byte| format!("{byte:02x}")).collect();
    format!(
        "{}-{}-{}-{}-{}",
        &hex[0..8],
        &hex[8..12],
        &hex[12..16],
        &hex[16..20],
        &hex[20..32]
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs::OpenOptions;

    #[test]
    fn formats_explicit_uuid_through_open_descriptor() {
        if Command::new("mke2fs").arg("-V").output().is_err()
            || Command::new("e2fsck").arg("-V").output().is_err()
        {
            return;
        }
        let directory = tempfile::tempdir().unwrap();
        let file = OpenOptions::new()
            .create_new(true)
            .read(true)
            .write(true)
            .open(directory.path().join("workspace.ext4"))
            .unwrap();
        format_workspace(&file, 64 * 1024 * 1024, "secondbox", [0x42; 16]).unwrap();
        let output = Command::new("tune2fs")
            .args(["-l", &crate::fd::reopen_path(file.as_raw_fd())])
            .output()
            .unwrap();
        let text = String::from_utf8_lossy(&output.stdout);
        assert!(text.contains("42424242-4242-4242-4242-424242424242"));
    }
}
