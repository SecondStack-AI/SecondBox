#![cfg(unix)]

use std::{
    fs::OpenOptions,
    os::{
        fd::{AsRawFd, FromRawFd},
        unix::{net::UnixStream, process::CommandExt},
    },
    process::{Child, Command, Stdio},
    thread,
    time::{Duration, Instant},
};

use secondbox_microsandbox_helper::{
    PROTOCOL_VERSION,
    frame::{read_frame, write_frame},
    protocol::{Envelope, FormatWorkspaceRequest, envelope::Message},
};

#[test]
fn inherited_descriptors_format_and_flush_explicit_uuid() {
    let directory = tempfile::tempdir().unwrap();
    let image_path = directory.path().join("workspace.ext4");
    let image = OpenOptions::new()
        .create_new(true)
        .read(true)
        .write(true)
        .open(&image_path)
        .unwrap();
    let (mut parent, child_socket) = UnixStream::pair().unwrap();
    let mut child = spawn(&child_socket, &image, None, "format-workspace");
    drop(child_socket);
    write_frame(
        &mut parent,
        &Envelope {
            protocol_version: PROTOCOL_VERSION,
            request_id: 1,
            message: Some(Message::FormatWorkspace(FormatWorkspaceRequest {
                logical_capacity_bytes: 64 * 1024 * 1024,
                label: "secondbox".into(),
                workspace_uuid: vec![0x52; 16],
            })),
            ..Default::default()
        },
    )
    .unwrap();
    let response = read_frame(&mut parent).unwrap().unwrap();
    assert!(matches!(response.message, Some(Message::Terminal(ref event)) if event.success));
    assert!(child.wait().unwrap().success());
    let output = Command::new("tune2fs")
        .arg("-l")
        .arg(image_path)
        .output()
        .unwrap();
    assert!(
        String::from_utf8_lossy(&output.stdout).contains("52525252-5252-5252-5252-525252525252")
    );
}

#[test]
fn lifecycle_pipe_eof_terminates_helper_within_bound() {
    let directory = tempfile::tempdir().unwrap();
    let image = OpenOptions::new()
        .create_new(true)
        .read(true)
        .write(true)
        .open(directory.path().join("workspace.ext4"))
        .unwrap();
    image.set_len(64 * 1024 * 1024).unwrap();
    let (_parent, child_socket) = UnixStream::pair().unwrap();
    let (lifecycle_read, lifecycle_write) = pipe().unwrap();
    let mut child = spawn(
        &child_socket,
        &image,
        Some(lifecycle_read.as_raw_fd()),
        "serve",
    );
    drop(child_socket);
    drop(lifecycle_read);
    let started = Instant::now();
    drop(lifecycle_write);
    loop {
        if let Some(status) = child.try_wait().unwrap() {
            assert!(status.success());
            assert!(started.elapsed() < Duration::from_secs(5));
            break;
        }
        assert!(
            started.elapsed() < Duration::from_secs(5),
            "helper survived parent-loss deadline"
        );
        thread::sleep(Duration::from_millis(10));
    }
}

fn spawn(
    control: &UnixStream,
    image: &std::fs::File,
    lifecycle: Option<i32>,
    action: &str,
) -> Child {
    let control_source = duplicate(control.as_raw_fd());
    let image_source = duplicate(image.as_raw_fd());
    let lifecycle_source = lifecycle.map(duplicate);
    let control_fd = control_source.as_raw_fd();
    let image_fd = image_source.as_raw_fd();
    let lifecycle_fd = lifecycle_source.as_ref().map(AsRawFd::as_raw_fd);
    let mut command = Command::new(env!("CARGO_BIN_EXE_secondbox-microsandbox-helper"));
    command
        .arg(action)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::piped());
    unsafe {
        command.pre_exec(move || {
            if libc::dup2(control_fd, 3) < 0 || libc::dup2(image_fd, 4) < 0 {
                return Err(std::io::Error::last_os_error());
            }
            if let Some(lifecycle_fd) = lifecycle_fd
                && libc::dup2(lifecycle_fd, 5) < 0
            {
                return Err(std::io::Error::last_os_error());
            }
            Ok(())
        });
    }
    command.spawn().unwrap()
}

fn duplicate(fd: i32) -> std::fs::File {
    let duplicate = unsafe { libc::fcntl(fd, libc::F_DUPFD_CLOEXEC, 10) };
    assert!(duplicate >= 10, "duplicate inherited test descriptor");
    unsafe { std::fs::File::from_raw_fd(duplicate) }
}

fn pipe() -> std::io::Result<(std::fs::File, std::fs::File)> {
    let mut descriptors = [0_i32; 2];
    if unsafe { libc::pipe2(descriptors.as_mut_ptr(), libc::O_CLOEXEC) } < 0 {
        return Err(std::io::Error::last_os_error());
    }
    Ok(unsafe {
        (
            std::fs::File::from_raw_fd(descriptors[0]),
            std::fs::File::from_raw_fd(descriptors[1]),
        )
    })
}
