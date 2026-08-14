//! Platform-correct inherited descriptor helpers.

use std::{
    io,
    os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd},
};

#[cfg(target_os = "linux")]
const REOPEN_ROOT: &str = "/proc/self/fd";
#[cfg(target_os = "macos")]
const REOPEN_ROOT: &str = "/dev/fd";

#[cfg(target_os = "linux")]
pub const WORKSPACE_DESCRIPTOR_PATH: &str = "/proc/self/fd/4";
#[cfg(target_os = "macos")]
pub const WORKSPACE_DESCRIPTOR_PATH: &str = "/dev/fd/4";

#[cfg(target_os = "linux")]
pub const AGENT_CONSOLE_DESCRIPTOR_PATH: &str = "/proc/self/fd/6";
#[cfg(target_os = "macos")]
pub const AGENT_CONSOLE_DESCRIPTOR_PATH: &str = "/dev/fd/6";

pub fn reopen_path(descriptor: RawFd) -> String {
    format!("{REOPEN_ROOT}/{descriptor}")
}

/// Creates a pipe whose endpoints never leak through a later `exec`.
pub fn pipe_cloexec(nonblocking: bool) -> io::Result<(OwnedFd, OwnedFd)> {
    let mut descriptors = [0_i32; 2];

    #[cfg(target_os = "linux")]
    {
        let mut flags = libc::O_CLOEXEC;
        if nonblocking {
            flags |= libc::O_NONBLOCK;
        }
        // SAFETY: `pipe2` initializes both descriptors on success.
        if unsafe { libc::pipe2(descriptors.as_mut_ptr(), flags) } < 0 {
            return Err(io::Error::last_os_error());
        }
    }

    #[cfg(target_os = "macos")]
    {
        // macOS has no `pipe2`. The helper creates these descriptors before it
        // starts the VMM, so no concurrent fork/exec can observe this narrow
        // setup window.
        // SAFETY: `pipe` initializes both descriptors on success.
        if unsafe { libc::pipe(descriptors.as_mut_ptr()) } < 0 {
            return Err(io::Error::last_os_error());
        }
    }

    // SAFETY: a successful pipe call returned two distinct, unowned fds.
    let read = unsafe { OwnedFd::from_raw_fd(descriptors[0]) };
    // SAFETY: a successful pipe call returned two distinct, unowned fds.
    let write = unsafe { OwnedFd::from_raw_fd(descriptors[1]) };

    #[cfg(target_os = "macos")]
    {
        set_cloexec(&read)?;
        set_cloexec(&write)?;
        if nonblocking {
            set_nonblocking(&read)?;
            set_nonblocking(&write)?;
        }
    }

    Ok((read, write))
}

#[cfg(target_os = "macos")]
fn set_cloexec(descriptor: &OwnedFd) -> io::Result<()> {
    // SAFETY: the descriptor is valid and F_SETFD accepts an integer mask.
    if unsafe { libc::fcntl(descriptor.as_raw_fd(), libc::F_SETFD, libc::FD_CLOEXEC) } < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

#[cfg(target_os = "macos")]
fn set_nonblocking(descriptor: &OwnedFd) -> io::Result<()> {
    // SAFETY: the descriptor is valid for both fcntl operations.
    let flags = unsafe { libc::fcntl(descriptor.as_raw_fd(), libc::F_GETFL) };
    if flags < 0 {
        return Err(io::Error::last_os_error());
    }
    if unsafe {
        libc::fcntl(
            descriptor.as_raw_fd(),
            libc::F_SETFL,
            flags | libc::O_NONBLOCK,
        )
    } < 0
    {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}
