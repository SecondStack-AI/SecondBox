use std::{
    fs::{File, OpenOptions},
    io::Read,
    os::{
        fd::{FromRawFd, RawFd},
        unix::net::UnixStream,
    },
    process,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
        mpsc,
    },
    thread,
    time::Duration,
};

use secondbox_microsandbox_helper::{
    HELPER_VERSION, PROTOCOL_VERSION, bounded_diagnostic,
    frame::{read_frame, write_frame},
    protocol::{self, DiagnosticEvent, Envelope, ReadyEvent, TerminalEvent, envelope::Message},
    state::ProtocolState,
    vmm::LaunchConfiguration,
    workspace::format_workspace,
};

const CONTROL_FD: RawFd = 3;
const WORKSPACE_FD: RawFd = 4;
const LIFECYCLE_FD: RawFd = 5;
const PARENT_LOSS_FLUSH_BOUND: Duration = Duration::from_secs(4);

fn main() {
    let action = std::env::args().nth(1).unwrap_or_default();
    let result = match action.as_str() {
        "serve" => serve(),
        "format-workspace" => serve_format_only(),
        _ => Err("usage: secondbox-microsandbox-helper serve|format-workspace".into()),
    };
    if let Err(error) = result {
        eprintln!(
            "SecondBox Microsandbox helper: {}",
            bounded_diagnostic(&error)
        );
        process::exit(1);
    }
}

fn serve() -> Result<(), String> {
    let mut control = inherited_socket(CONTROL_FD)?;
    let workspace = inherited_file(WORKSPACE_FD, true)?;
    let shutting_down = Arc::new(AtomicBool::new(false));
    monitor_parent(
        shutting_down.clone(),
        workspace.try_clone().map_err(|error| error.to_string())?,
    )?;
    let mut state = ProtocolState::default();
    while !shutting_down.load(Ordering::Acquire) {
        let Some(envelope) = read_frame(&mut control).map_err(|error| error.to_string())? else {
            break;
        };
        if let Err(error) = state.admit(&envelope) {
            write_diagnostic(
                &mut control,
                &envelope,
                "protocol_rejected",
                &error.to_string(),
            )?;
            continue;
        }
        match envelope.message.as_ref() {
            Some(Message::Start(start)) => {
                let launch =
                    LaunchConfiguration::from_start(start).map_err(|error| error.to_string())?;
                launch.build().map_err(|error| {
                    format!("SecondBox Microsandbox helper libkrun configuration: {error}")
                })?;
                write_frame(
                    &mut control,
                    &Envelope {
                        protocol_version: PROTOCOL_VERSION,
                        request_id: envelope.request_id,
                        message: Some(Message::Ready(ReadyEvent {
                            helper_version: HELPER_VERSION.into(),
                            dependency_version: "microsandbox-0.6.8/msb_krun-0.1.30".into(),
                            host_platform: format!(
                                "{}-{}",
                                std::env::consts::OS,
                                std::env::consts::ARCH
                            ),
                            agent_protocol_generation: 1,
                            agent_features: vec![
                                "exec-streaming".into(),
                                "file-streaming".into(),
                                "pty".into(),
                                "tcp".into(),
                            ],
                            operations: supported_operations()
                                .into_iter()
                                .map(|operation| operation as i32)
                                .collect(),
                            materialization_digest: start.materialization_digest.clone(),
                        })),
                        ..Default::default()
                    },
                )
                .map_err(|error| error.to_string())?;
            }
            Some(Message::FormatWorkspace(request)) => {
                let uuid: [u8; 16] = request
                    .workspace_uuid
                    .as_slice()
                    .try_into()
                    .map_err(|_| "Workspace UUID must contain 16 bytes")?;
                format_workspace(
                    &workspace,
                    request.logical_capacity_bytes,
                    &request.label,
                    uuid,
                )
                .map_err(|error| error.to_string())?;
                write_terminal(&mut control, &envelope, true, 0, "formatted")?;
            }
            Some(Message::Shutdown(_)) => {
                workspace.sync_all().map_err(|error| error.to_string())?;
                write_terminal(&mut control, &envelope, true, 0, "stopped")?;
                return Ok(());
            }
            Some(Message::Cancel(_))
            | Some(Message::Exec(_))
            | Some(Message::File(_))
            | Some(Message::Pty(_))
            | Some(Message::Tcp(_))
            | Some(Message::StreamData(_))
            | Some(Message::StreamCredit(_)) => {
                write_diagnostic(
                    &mut control,
                    &envelope,
                    "operation_pending_vmm",
                    "operation is admitted but requires the active guest relay",
                )?;
            }
            _ => write_diagnostic(
                &mut control,
                &envelope,
                "invalid_direction",
                "event is not accepted from the runner",
            )?,
        }
    }
    workspace.sync_all().map_err(|error| error.to_string())?;
    Ok(())
}

fn serve_format_only() -> Result<(), String> {
    let mut control = inherited_socket(CONTROL_FD)?;
    let workspace = inherited_file(WORKSPACE_FD, true)?;
    let envelope = read_frame(&mut control)
        .map_err(|error| error.to_string())?
        .ok_or("format request is required")?;
    if envelope.protocol_version != PROTOCOL_VERSION
        || envelope.request_id == 0
        || envelope.stream_id != 0
        || envelope.sequence != 0
    {
        return Err("format request envelope is invalid".into());
    }
    let Some(Message::FormatWorkspace(request)) = envelope.message.as_ref() else {
        return Err("format-workspace request is required".into());
    };
    let uuid: [u8; 16] = request
        .workspace_uuid
        .as_slice()
        .try_into()
        .map_err(|_| "Workspace UUID must contain 16 bytes")?;
    format_workspace(
        &workspace,
        request.logical_capacity_bytes,
        &request.label,
        uuid,
    )
    .map_err(|error| error.to_string())?;
    write_terminal(&mut control, &envelope, true, 0, "formatted")
}

fn supported_operations() -> Vec<protocol::Operation> {
    use protocol::Operation::*;
    vec![
        Start,
        Exec,
        FileRead,
        FileWrite,
        FileStat,
        FileList,
        FileMkdir,
        FileRemove,
        Pty,
        Tcp,
        Shutdown,
        FormatWorkspace,
    ]
}

fn write_terminal(
    control: &mut UnixStream,
    request: &Envelope,
    success: bool,
    exit_code: i32,
    reason: &str,
) -> Result<(), String> {
    write_frame(
        control,
        &Envelope {
            protocol_version: PROTOCOL_VERSION,
            request_id: request.request_id,
            message: Some(Message::Terminal(TerminalEvent {
                success,
                exit_code,
                reason: bounded_diagnostic(reason),
            })),
            ..Default::default()
        },
    )
    .map_err(|error| error.to_string())
}

fn write_diagnostic(
    control: &mut UnixStream,
    request: &Envelope,
    code: &str,
    text: &str,
) -> Result<(), String> {
    write_frame(
        control,
        &Envelope {
            protocol_version: PROTOCOL_VERSION,
            request_id: request.request_id,
            stream_id: request.stream_id,
            message: Some(Message::Diagnostic(DiagnosticEvent {
                code: code.into(),
                text: bounded_diagnostic(text),
            })),
            ..Default::default()
        },
    )
    .map_err(|error| error.to_string())
}

fn monitor_parent(shutting_down: Arc<AtomicBool>, workspace: File) -> Result<(), String> {
    let mut lifecycle = inherited_file(LIFECYCLE_FD, false)?;
    thread::Builder::new()
        .name("secondbox-parent-watchdog".into())
        .spawn(move || {
            let mut byte = [0_u8; 1];
            while lifecycle.read(&mut byte).is_ok_and(|count| count != 0) {}
            shutting_down.store(true, Ordering::Release);
            let (completed, flushed) = mpsc::sync_channel(1);
            if thread::Builder::new()
                .name("secondbox-parent-loss-flush".into())
                .spawn(move || {
                    let _ = completed.send(workspace.sync_all());
                })
                .is_err()
            {
                process::exit(1);
            }
            let status = match flushed.recv_timeout(PARENT_LOSS_FLUSH_BOUND) {
                Ok(Ok(())) => 0,
                Ok(Err(_)) | Err(_) => 1,
            };
            process::exit(status);
        })
        .map_err(|error| format!("start parent watchdog: {error}"))?;
    Ok(())
}

fn inherited_socket(fd: RawFd) -> Result<UnixStream, String> {
    if fd < 3 {
        return Err("inherited control descriptor is invalid".into());
    }
    // SAFETY: the runner transfers exclusive ownership of this fixed inherited descriptor.
    Ok(unsafe { UnixStream::from_raw_fd(fd) })
}

fn inherited_file(fd: RawFd, writable: bool) -> Result<File, String> {
    let path = format!("/proc/self/fd/{fd}");
    OpenOptions::new()
        .read(true)
        .write(writable)
        .open(path)
        .map_err(|error| format!("open inherited descriptor {fd}: {error}"))
}
